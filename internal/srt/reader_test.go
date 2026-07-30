package srt

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// cue builds an expected cue. Lines is always non-nil, matching the reader.
func cue(index string, start, end time.Duration, lines ...string) Cue {
	if lines == nil {
		lines = []string{}
	}
	return Cue{Index: index, Start: start, End: end, Lines: lines}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func parseFixture(t *testing.T, name string) *File {
	t.Helper()
	f, err := ParseBytes(readFixture(t, name))
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", name, err)
	}
	return f
}

// fixtureCases pins down the exact parse of every fixture. The point of the
// package is that nothing is silently transformed, so the expectations are
// spelled out in full rather than spot-checked.
func fixtureCases() []struct {
	name       string
	cues       []Cue
	recovered  []int // indices of cues with RecoveredBoundary set
	lineEnding LineEnding
	trailing   int
	bom        []byte
	warnings   []string // substrings, one per expected warning, in order
} {
	return []struct {
		name       string
		cues       []Cue
		recovered  []int
		lineEnding LineEnding
		trailing   int
		bom        []byte
		warnings   []string
	}{
		{
			name: "basic.srt",
			cues: []Cue{
				cue("1", time.Second, 4*time.Second, "Hello, world."),
				cue("2", 5500*time.Millisecond, 7250*time.Millisecond, "Second cue,", "on two lines."),
				cue("3", time.Minute, time.Minute+2*time.Second, "Third."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			// A dot separator, one and two millisecond digits (padded as
			// the decimal fractions they are), and more than three digits.
			name: "ms_separators.srt",
			cues: []Cue{
				cue("1", time.Second, 2500*time.Millisecond, "Dot as the millisecond separator."),
				cue("2", 3500*time.Millisecond, 4050*time.Millisecond, "One and two millisecond digits."),
				cue("3", 5123*time.Millisecond, 6999*time.Millisecond, "More than three millisecond digits."),
			},
			lineEnding: LF, trailing: 1,
			warnings: []string{"truncated to three digits"},
		},
		{
			// The single most important case in the package: HH:MM:SS
			// with no milliseconds must not be read as MM:SS:mmm.
			name: "colon_forms.srt",
			cues: []Cue{
				cue("1", 90*time.Second, 95*time.Second, "HH:MM:SS with no milliseconds at all."),
				cue("2", 96500*time.Millisecond, 98750*time.Millisecond, "HH:MM:SS:mmm in the very same file."),
				cue("3", 2*time.Minute, 2*time.Minute+5*time.Second, "MM:SS only."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "long_hours.srt",
			cues: []Cue{
				cue("1", 100*time.Hour, 100*time.Hour+2*time.Second, "Three digit hours."),
				cue("2", 25*time.Hour+30*time.Minute+500*time.Millisecond,
					25*time.Hour+30*time.Minute+3*time.Second, "Past the twenty-four hour mark."),
				cue("3", 90*time.Minute, 90*time.Minute+5*time.Second,
					"Ninety minutes, out of range but kept."),
			},
			lineEnding: LF, trailing: 1,
			warnings: []string{"minutes value 90", "minutes value 90"},
		},
		{
			name: "coords.srt",
			cues: []Cue{
				cue("1", time.Second, 3*time.Second, "Legacy on-screen coordinates."),
				cue("2", 4*time.Second, 5*time.Second, "No spaces around the arrow."),
				cue("3", 6*time.Second, 7*time.Second, "An extra dash in the arrow."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "no_index.srt",
			cues: []Cue{
				cue("", time.Second, 2*time.Second, "No index line at all."),
				cue("", 3*time.Second, 4*time.Second, "None here either,", "on two lines."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "missing_blank.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "First cue."),
				cue("2", 3*time.Second, 4*time.Second, "Second cue, with no blank line before it."),
				cue("3", 5*time.Second, 6*time.Second, "Third cue, properly separated."),
			},
			recovered:  []int{1},
			lineEnding: LF, trailing: 1,
			warnings: []string{`missing blank line before cue "2"`},
		},
		{
			// The ffmpeg regression: the numeric last line of a cue
			// followed by an index-less cue must survive as text.
			name: "numeric_last_line.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Call me at", "555"),
				cue("", 3*time.Second, 4*time.Second, "The cue above must keep its 555."),
				cue("3", 5*time.Second, 6*time.Second, "42"),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "empty_cue.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second),
				cue("2", 3*time.Second, 4*time.Second),
				cue("3", 5*time.Second, 6*time.Second, "Ordinary text."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "out_of_order.srt",
			cues: []Cue{
				cue("1", 10*time.Second, 12*time.Second, "The latest cue comes first."),
				cue("2", 2*time.Second, 8*time.Second, "An earlier cue, overlapping the next one."),
				cue("3", 5*time.Second, 9*time.Second, "Overlaps the cue above."),
				cue("4", 9*time.Second, 8*time.Second, "Ends before it starts."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "weird_indices.srt",
			cues: []Cue{
				cue("0", time.Second, 2*time.Second, "Zero based."),
				cue("0", 3*time.Second, 4*time.Second, "Duplicate index."),
				cue("007", 5*time.Second, 6*time.Second, "Zero padded index."),
				cue("3", 7*time.Second, 8*time.Second, "Gapped index."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "leading_spaces.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "    Four leading spaces.", "  Two leading spaces."),
				cue("2", 3*time.Second, 4*time.Second,
					"Trailing spaces follow.   ", "\tA leading tab.", "   "),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "markup.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "<i>Italic</i> <i>and</i> the space between them."),
				cue("2", 3*time.Second, 4*time.Second,
					`<font color="#ffffff" face="Arial" size="18">Coloured</font>`),
				cue("3", 5*time.Second, 6*time.Second, `{\an8}Pinned to the top of the screen.`),
				cue("4", 7*time.Second, 8*time.Second, "Tom & Jerry, 5 > 3, 2 < 4, &amp; &gt; stay as typed."),
				cue("5", 9*time.Second, 10*time.Second, "<b>Bold</b> and <u>underlined</u>",
					`{\pos(192,230)}Positioned.`),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "arrow_in_text.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Point A --> Point B", "00:00:09,000 --> 00:00:10,000"),
				cue("2", 3*time.Second, 4*time.Second, "-- What now?", "--> That way."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "blank_in_cue.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Before the blank line", "", "after the blank line."),
				cue("2", 3*time.Second, 4*time.Second,
					"Two blank lines before this cue collapse into one."),
			},
			lineEnding: LF, trailing: 1,
		},
		{
			name: "crlf.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Carriage return, line feed."),
				cue("2", 3*time.Second, 4*time.Second, "Second cue."),
			},
			lineEnding: CRLF, trailing: 2,
		},
		{
			name: "cr.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Classic Mac line endings."),
				cue("2", 3*time.Second, 4*time.Second, "Second cue,", "on two lines."),
			},
			lineEnding: CR, trailing: 1,
		},
		{
			name: "mixed_endings.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "This cue uses CRLF."),
				cue("2", 3*time.Second, 4*time.Second, "This one uses LF."),
				cue("3", 5*time.Second, 6*time.Second, "CRLF again."),
			},
			lineEnding: Mixed, trailing: 1,
			warnings: []string{"mixed line endings: 4 LF, 7 CRLF, 0 CR"},
		},
		{
			name: "bom.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "The file starts with a UTF-8 BOM."),
				cue("2", 3*time.Second, 4*time.Second, "Καλημέρα."),
			},
			lineEnding: LF, trailing: 1, bom: []byte{0xEF, 0xBB, 0xBF},
		},
		{
			name:       "empty.srt",
			cues:       nil,
			lineEnding: LF, trailing: 0,
		},
		{
			name: "no_trailing_newline.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "No newline at the end of this file."),
			},
			lineEnding: LF, trailing: 0,
		},
		{
			name: "many_trailing_newlines.srt",
			cues: []Cue{
				cue("1", time.Second, 2*time.Second, "Four trailing newlines."),
			},
			lineEnding: LF, trailing: 4,
		},
	}
}

func TestParseFixtures(t *testing.T) {
	t.Parallel()

	for _, tc := range fixtureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := parseFixture(t, tc.name)

			want := make([]Cue, len(tc.cues))
			copy(want, tc.cues)
			for _, i := range tc.recovered {
				want[i].RecoveredBoundary = true
			}
			if len(want) == 0 {
				want = nil
			}
			if !reflect.DeepEqual(f.Cues, want) {
				t.Errorf("cues:\n got %#v\nwant %#v", f.Cues, want)
			}
			if f.LineEnding != tc.lineEnding {
				t.Errorf("LineEnding = %v, want %v", f.LineEnding, tc.lineEnding)
			}
			if f.TrailingNewlines != tc.trailing {
				t.Errorf("TrailingNewlines = %d, want %d", f.TrailingNewlines, tc.trailing)
			}
			if !reflect.DeepEqual(f.BOM, tc.bom) {
				t.Errorf("BOM = %v, want %v", f.BOM, tc.bom)
			}
			if len(f.Warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %q, want %d matching %q", f.Warnings, len(tc.warnings), tc.warnings)
			}
			for i, want := range tc.warnings {
				if !strings.Contains(f.Warnings[i], want) {
					t.Errorf("warning %d = %q, want it to contain %q", i, f.Warnings[i], want)
				}
			}
		})
	}
}

// TestParseNumericLastLineKeptAsText is the regression ffmpeg 8.1.2 fails: the
// numeric line is the last text line of a cue and the next cue has no index.
func TestParseNumericLastLineKeptAsText(t *testing.T) {
	t.Parallel()

	const in = "1\n00:00:01,000 --> 00:00:02,000\nRoom\n237\n\n" +
		"00:00:03,000 --> 00:00:04,000\nSecond.\n"

	f, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(f.Cues) != 2 {
		t.Fatalf("got %d cues, want 2: %#v", len(f.Cues), f.Cues)
	}
	if got, want := f.Cues[0].Lines, []string{"Room", "237"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cue 1 lines = %q, want %q", got, want)
	}
	if f.Cues[1].Index != "" {
		t.Errorf("cue 2 index = %q, want empty", f.Cues[1].Index)
	}
	if f.Cues[1].RecoveredBoundary {
		t.Error("cue 2 boundary was explicit, RecoveredBoundary should be false")
	}
}

func TestParseIndexRule(t *testing.T) {
	t.Parallel()

	// Every input opens with a well-formed cue so that the line under test
	// is never the first line of the file.
	const (
		head = "1\n00:00:01,000 --> 00:00:02,000\nfirst\n\n"
		ts2  = "00:00:05,000 --> 00:00:06,000"
	)

	tests := []struct {
		name string
		in   string
		want []Cue
	}{
		{
			name: "digits followed by a timing line are an index",
			in:   head + "12\n" + ts2 + "\ntext\n",
			want: []Cue{
				cue("1", time.Second, 2*time.Second, "first"),
				cue("12", 5*time.Second, 6*time.Second, "text"),
			},
		},
		{
			// A leading space breaks ^\d+$ on the untrimmed line, so
			// the line is text, and the timing line after it is then
			// mid-cue text as well. Sharp, but decidable and local:
			// the damage cannot spread past the next blank line.
			name: "a leading space is not an index",
			in:   head + " 555\n" + ts2 + "\ntext\n",
			want: []Cue{
				cue("1", time.Second, 2*time.Second, "first", "", " 555", ts2, "text"),
			},
		},
		{
			name: "a negative number is not an index",
			in:   head + "-5\n" + ts2 + "\ntext\n",
			want: []Cue{
				cue("1", time.Second, 2*time.Second, "first", "", "-5", ts2, "text"),
			},
		},
		{
			name: "a numeric first text line stays text",
			in:   "1\n00:00:01,000 --> 00:00:02,000\n99\nmore\n",
			want: []Cue{
				cue("1", time.Second, 2*time.Second, "99", "more"),
			},
		},
		{
			name: "digits not followed by a timing line stay text",
			in:   head + "42\nmore\n",
			want: []Cue{
				cue("1", time.Second, 2*time.Second, "first", "", "42", "more"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, err := ParseBytes([]byte(tc.in))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			if !reflect.DeepEqual(f.Cues, tc.want) {
				t.Errorf("cues:\n got %#v\nwant %#v", f.Cues, tc.want)
			}
		})
	}
}

func TestParseRecoveredBoundary(t *testing.T) {
	t.Parallel()

	f := parseFixture(t, "missing_blank.srt")
	if !f.Cues[1].RecoveredBoundary {
		t.Error("cue 2 should have RecoveredBoundary set")
	}
	if f.Cues[0].RecoveredBoundary || f.Cues[2].RecoveredBoundary {
		t.Error("only cue 2 should have a recovered boundary")
	}
	if len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "line 4") {
		t.Errorf("warnings = %q, want one naming line 4", f.Warnings)
	}
}

// A bare timing line in the middle of cue text is not a boundary unless the
// previous line can be consumed as an index; otherwise a text line that happens
// to be a timestamp would split a cue in two.
func TestParseBareTimestampMidTextIsNotABoundary(t *testing.T) {
	t.Parallel()

	const in = "1\n00:00:01,000 --> 00:00:02,000\ntext\n00:00:03,000 --> 00:00:04,000\nmore\n"

	f, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(f.Cues) != 1 {
		t.Fatalf("got %d cues, want 1: %#v", len(f.Cues), f.Cues)
	}
	want := []string{"text", "00:00:03,000 --> 00:00:04,000", "more"}
	if !reflect.DeepEqual(f.Cues[0].Lines, want) {
		t.Errorf("lines = %q, want %q", f.Cues[0].Lines, want)
	}
}

func TestParseEmptyInput(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "\n", "\n\n\n", "\r\n"} {
		f, err := ParseBytes([]byte(in))
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", in, err)
		}
		if len(f.Cues) != 0 {
			t.Errorf("ParseBytes(%q): got %d cues, want 0", in, len(f.Cues))
		}
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "text before the first cue",
			in:   "Some notes about this rip\n\n1\n00:00:01,000 --> 00:00:02,000\ntext\n",
			want: "line 1",
		},
		{
			name: "broken timing line with no cue open",
			in:   "1\n00:00:0X,000 --> 00:00:02,000\ntext\n",
			want: "line 2",
		},
		{
			name: "index with nothing after it",
			in:   "5",
			want: "line 1",
		},
		{
			name: "not a subtitle file at all",
			in:   "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\ntext\n",
			want: "line 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseBytes([]byte(tc.in))
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// A timing-like line that fails to parse but sits where a cue could start is
// kept as text and reported, never dropped.
func TestParseBrokenTimestampAtBoundaryIsKept(t *testing.T) {
	t.Parallel()

	const in = "1\n00:00:01,000 --> 00:00:02,000\ntext\n\n00:00:0X,000 --> 00:00:04,000\nmore\n"

	f, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(f.Cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(f.Cues))
	}
	want := []string{"text", "", "00:00:0X,000 --> 00:00:04,000", "more"}
	if !reflect.DeepEqual(f.Cues[0].Lines, want) {
		t.Errorf("lines = %q, want %q", f.Cues[0].Lines, want)
	}
	if len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "line 5") {
		t.Errorf("warnings = %q, want one naming line 5", f.Warnings)
	}
}

func TestParseReaderError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	if _, err := Parse(iotest.ErrReader(wantErr)); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestParseMatchesParseBytes(t *testing.T) {
	t.Parallel()

	data := readFixture(t, "basic.srt")
	fromReader, err := Parse(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fromBytes, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if !reflect.DeepEqual(fromReader, fromBytes) {
		t.Error("Parse and ParseBytes disagree")
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in       string
		want     []string
		counts   [3]int
		trailing int
	}{
		{in: "", want: nil},
		{in: "a", want: []string{"a"}},
		{in: "a\n", want: []string{"a"}, counts: [3]int{LF: 1}, trailing: 1},
		{in: "a\n\n", want: []string{"a", ""}, counts: [3]int{LF: 2}, trailing: 2},
		{in: "a\r\nb", want: []string{"a", "b"}, counts: [3]int{CRLF: 1}},
		{in: "a\rb\r", want: []string{"a", "b"}, counts: [3]int{CR: 2}, trailing: 1},
		{in: "a\r\n\r\n", want: []string{"a", ""}, counts: [3]int{CRLF: 2}, trailing: 2},
		{in: "a\r\nb\nc\r", want: []string{"a", "b", "c"}, counts: [3]int{LF: 1, CRLF: 1, CR: 1}, trailing: 1},
		{in: "\n\n\n", want: []string{"", "", ""}, counts: [3]int{LF: 3}, trailing: 3},
	}

	for _, tc := range tests {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(tc.in, "\r", `\r`), "\n", `\n`), func(t *testing.T) {
			t.Parallel()

			lines, counts, trailing := splitLines(tc.in)
			if !reflect.DeepEqual(lines, tc.want) {
				t.Errorf("lines = %q, want %q", lines, tc.want)
			}
			if counts != tc.counts {
				t.Errorf("counts = %v, want %v", counts, tc.counts)
			}
			if trailing != tc.trailing {
				t.Errorf("trailing = %d, want %d", trailing, tc.trailing)
			}
		})
	}
}

func TestIsDigits(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]bool{
		"1": true, "007": true, "0": true, "12345": true,
		"": false, " 1": false, "1 ": false, "-1": false, "1.0": false, "a": false, "１": false,
	} {
		if got := isDigits(in); got != want {
			t.Errorf("isDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLineEndingString(t *testing.T) {
	t.Parallel()

	for e, want := range map[LineEnding]string{
		LF: "LF", CRLF: "CRLF", CR: "CR", Mixed: "Mixed", LineEnding(9): "LineEnding(9)",
	} {
		if got := e.String(); got != want {
			t.Errorf("LineEnding(%d).String() = %q, want %q", int(e), got, want)
		}
	}
}
