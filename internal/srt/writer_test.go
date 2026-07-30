package srt

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeString(t *testing.T, f *File, opts WriteOptions) (string, []string) {
	t.Helper()
	b, warns, err := WriteBytes(f, opts)
	if err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	return string(b), warns
}

func TestWriteGolden(t *testing.T) {
	t.Parallel()

	f := &File{
		Cues: []Cue{
			{Index: "7", Start: 90 * time.Second, End: 95 * time.Second, Lines: []string{"first", "second"}},
			{Index: "", Start: 100 * time.Hour, End: 100*time.Hour + 500*time.Millisecond},
			{Index: "0", Start: 0, End: 40 * time.Millisecond, Lines: []string{"  indented  "}},
		},
		TrailingNewlines: 2,
	}

	const wantRenumbered = "1\n00:01:30,000 --> 00:01:35,000\nfirst\nsecond\n\n" +
		"2\n100:00:00,000 --> 100:00:00,500\n\n" +
		"3\n00:00:00,000 --> 00:00:00,040\n  indented  \n\n"
	if got, _ := writeString(t, f, WriteOptions{}); got != wantRenumbered {
		t.Errorf("renumbered:\n got %q\nwant %q", got, wantRenumbered)
	}

	// KeepIndices re-emits the raw token; the index-less cue falls back to
	// its sequence number.
	const wantKept = "7\n00:01:30,000 --> 00:01:35,000\nfirst\nsecond\n\n" +
		"2\n100:00:00,000 --> 100:00:00,500\n\n" +
		"0\n00:00:00,000 --> 00:00:00,040\n  indented  \n\n"
	if got, _ := writeString(t, f, WriteOptions{KeepIndices: true}); got != wantKept {
		t.Errorf("kept indices:\n got %q\nwant %q", got, wantKept)
	}
}

// The fixtures that are already canonical must come back byte for byte.
func TestWriteByteExact(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"basic.srt",
		"markup.srt",
		"leading_spaces.srt",
		"arrow_in_text.srt",
		"crlf.srt",
		"cr.srt",
		"bom.srt",
		"many_trailing_newlines.srt",
		"weird_indices.srt",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			in := readFixture(t, name)
			f := parseFixture(t, name)
			got, _ := writeString(t, f, WriteOptions{KeepIndices: true})
			if got != string(in) {
				t.Errorf("round trip is not byte exact:\n got %q\nwant %q", got, string(in))
			}
		})
	}
}

func TestWriteRenumbers(t *testing.T) {
	t.Parallel()

	f := parseFixture(t, "weird_indices.srt")
	got, _ := writeString(t, f, WriteOptions{})

	reparsed, err := ParseBytes([]byte(got))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	for i, c := range reparsed.Cues {
		if want := strconv.Itoa(i + 1); c.Index != want {
			t.Errorf("cue %d index = %q, want %q", i+1, c.Index, want)
		}
	}
	if strings.Contains(got, "007") {
		t.Errorf("a raw index token leaked into a renumbered file:\n%s", got)
	}
}

func TestWriteBOM(t *testing.T) {
	t.Parallel()

	yes, no := true, false
	bom := string(utf8BOM)

	tests := []struct {
		name    string
		fixture string
		opts    WriteOptions
		want    bool
	}{
		{name: "inherit present", fixture: "bom.srt", want: true},
		{name: "inherit absent", fixture: "basic.srt", want: false},
		{name: "force on", fixture: "basic.srt", opts: WriteOptions{BOM: &yes}, want: true},
		{name: "force off", fixture: "bom.srt", opts: WriteOptions{BOM: &no}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, _ := writeString(t, parseFixture(t, tc.fixture), tc.opts)
			if has := strings.HasPrefix(got, bom); has != tc.want {
				t.Errorf("BOM present = %v, want %v", has, tc.want)
			}
		})
	}
}

func TestWriteLineEndings(t *testing.T) {
	t.Parallel()

	crlf, cr, lf, mixed := CRLF, CR, LF, Mixed

	tests := []struct {
		name    string
		fixture string
		opts    WriteOptions
		want    string
	}{
		{name: "inherit LF", fixture: "basic.srt", want: "\n"},
		{name: "inherit CRLF", fixture: "crlf.srt", want: "\r\n"},
		{name: "inherit CR", fixture: "cr.srt", want: "\r"},
		{name: "force CRLF", fixture: "basic.srt", opts: WriteOptions{LineEnding: &crlf}, want: "\r\n"},
		{name: "force CR", fixture: "basic.srt", opts: WriteOptions{LineEnding: &cr}, want: "\r"},
		{name: "force LF", fixture: "crlf.srt", opts: WriteOptions{LineEnding: &lf}, want: "\n"},
		// Asking for Mixed is meaningless; the file's own majority wins.
		{name: "force Mixed", fixture: "crlf.srt", opts: WriteOptions{LineEnding: &mixed}, want: "\r\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, _ := writeString(t, parseFixture(t, tc.fixture), tc.opts)
			if !strings.Contains(got, tc.want) {
				t.Errorf("output does not use %q:\n%q", tc.want, got)
			}
			// Once the wanted terminator is removed no CR or LF may
			// be left anywhere.
			if rest := strings.ReplaceAll(got, tc.want, "\x00"); strings.ContainsAny(rest, "\r\n") {
				t.Errorf("output mixes line endings into a %q file:\n%q", tc.want, got)
			}
		})
	}
}

// A hand-built File has no line ending counts to take a majority of, and an
// out-of-range value must not index past the terminator table.
func TestResolveLineEndingFallsBackToLF(t *testing.T) {
	t.Parallel()

	bogus := LineEnding(99)
	negative := LineEnding(-1)

	for name, f := range map[string]*File{
		"mixed without counts": {LineEnding: Mixed},
		"out of range":         {LineEnding: bogus},
	} {
		if eol, _ := resolveLineEnding(f, WriteOptions{}); eol != "\n" {
			t.Errorf("%s: eol = %q, want LF", name, eol)
		}
	}
	for name, opts := range map[string]WriteOptions{
		"forced out of range": {LineEnding: &bogus},
		"forced negative":     {LineEnding: &negative},
	} {
		if eol, _ := resolveLineEnding(&File{}, opts); eol != "\n" {
			t.Errorf("%s: eol = %q, want LF", name, eol)
		}
	}
}

func TestWriteMixedPicksMajorityAndWarns(t *testing.T) {
	t.Parallel()

	f := parseFixture(t, "mixed_endings.srt")
	got, warns := writeString(t, f, WriteOptions{})
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("output is not all CRLF:\n%q", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "mixed line endings") {
		t.Errorf("warnings = %q, want one about mixed line endings", warns)
	}
}

func TestWriteTrailingNewlines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture string
		want    string
	}{
		{fixture: "many_trailing_newlines.srt", want: "Four trailing newlines.\n\n\n\n"},
		// A file with no trailing newline gets exactly one.
		{fixture: "no_trailing_newline.srt", want: "No newline at the end of this file.\n"},
		{fixture: "basic.srt", want: "Third.\n"},
		{fixture: "crlf.srt", want: "Second cue.\r\n\r\n"},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			got, _ := writeString(t, parseFixture(t, tc.fixture), WriteOptions{})
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("output does not end with %q:\n%q", tc.want, got)
			}
		})
	}
}

func TestWriteEmptyFile(t *testing.T) {
	t.Parallel()

	if got, _ := writeString(t, &File{}, WriteOptions{}); got != "" {
		t.Errorf("empty file wrote %q, want nothing", got)
	}
	// A file with no cues but a trailing newline count still writes
	// nothing: a lone newline would be an invention.
	if got, _ := writeString(t, &File{TrailingNewlines: 3}, WriteOptions{}); got != "" {
		t.Errorf("empty file wrote %q, want nothing", got)
	}
	if got, _ := writeString(t, &File{BOM: utf8BOM}, WriteOptions{}); got != string(utf8BOM) {
		t.Errorf("empty file with a BOM wrote %q, want just the BOM", got)
	}
}

// Nothing is escaped: markup, ampersands and angle brackets go out as they came
// in, and a text line that contains an arrow is not quoted or altered.
func TestWriteDoesNotEscape(t *testing.T) {
	t.Parallel()

	f := &File{Cues: []Cue{{
		Index: "1",
		Lines: []string{`Tom & Jerry <i>&amp;</i> 5 > 3`, `{\an8}--> that way`},
	}}}
	got, _ := writeString(t, f, WriteOptions{})
	for _, want := range []string{`Tom & Jerry <i>&amp;</i> 5 > 3`, `{\an8}--> that way`} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%q", want, got)
		}
	}
}

func TestWriteTimingWarnings(t *testing.T) {
	t.Parallel()

	f := &File{Cues: []Cue{
		{Start: -time.Second, End: 2 * time.Second},
		{Start: 5 * time.Second, End: 4 * time.Second},
	}}
	got, warns := writeString(t, f, WriteOptions{})
	if len(warns) != 2 {
		t.Fatalf("warnings = %q, want two", warns)
	}
	if !strings.Contains(warns[0], "negative") || !strings.Contains(warns[1], "before start") {
		t.Errorf("warnings = %q", warns)
	}
	if !strings.Contains(got, "00:00:00,000 --> 00:00:02,000") {
		t.Errorf("negative start was not clamped:\n%q", got)
	}
}

type failingWriter struct {
	err   error
	after int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, w.err
	}
	w.after--
	return len(p), nil
}

func TestWriteError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("disk full")
	f := parseFixture(t, "basic.srt")

	// Small file: the error surfaces from the final flush.
	if _, err := Write(&failingWriter{err: wantErr}, f, WriteOptions{}); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}

	// The BOM is written through the same guarded writer.
	if _, err := Write(&failingWriter{err: wantErr}, parseFixture(t, "bom.srt"), WriteOptions{}); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}

	// Large file: the error surfaces mid-run and the writer stops.
	big := &File{TrailingNewlines: 2}
	for i := 0; i < 500; i++ {
		big.Cues = append(big.Cues, Cue{Lines: []string{strings.Repeat("x", 60)}})
	}
	if _, err := Write(&failingWriter{err: wantErr}, big, WriteOptions{}); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}
}
