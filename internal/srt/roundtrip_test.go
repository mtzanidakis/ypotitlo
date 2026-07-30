package srt

import (
	"path/filepath"
	"reflect"
	"testing"
)

// fixtureNames lists every file in testdata. Adding a fixture automatically
// subjects it to the round-trip invariants below.
func fixtureNames(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("testdata", "*.srt"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) < 20 {
		t.Fatalf("only %d fixtures found, the test data is missing", len(names))
	}
	for i, n := range names {
		names[i] = filepath.Base(n)
	}
	return names
}

// TestFixturesAreIdempotent is the central invariant of the package. Parsing
// and writing is not always byte-exact — a malformed boundary is repaired, a
// non-canonical timestamp is normalised, blank lines at the end of a cue's text
// collapse into the separator — but it must reach a fixed point immediately:
// writing what was parsed from a written file reproduces those same bytes, and
// re-parsing reproduces the same File.
func TestFixturesAreIdempotent(t *testing.T) {
	t.Parallel()

	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			opts := WriteOptions{KeepIndices: true}

			f1 := parseFixture(t, name)
			b1, _, err := WriteBytes(f1, opts)
			if err != nil {
				t.Fatalf("first write: %v", err)
			}

			f2, err := ParseBytes(b1)
			if err != nil {
				t.Fatalf("reparse: %v\n%q", err, b1)
			}
			b2, _, err := WriteBytes(f2, opts)
			if err != nil {
				t.Fatalf("second write: %v", err)
			}
			if string(b1) != string(b2) {
				t.Fatalf("writing is not idempotent:\n first %q\nsecond %q", b1, b2)
			}

			f3, err := ParseBytes(b2)
			if err != nil {
				t.Fatalf("second reparse: %v", err)
			}
			if !reflect.DeepEqual(f2, f3) {
				t.Errorf("parsing is not idempotent:\n got %#v\nwant %#v", f3, f2)
			}
		})
	}
}

// TestFixturesPreserveContent is the invariant that matters to a translation:
// the cue count, the parsed durations and every byte of every text line survive
// a round trip. Timestamps are deliberately normalised on output, so the
// comparison is on the parsed durations, not on the bytes of the timing lines.
func TestFixturesPreserveContent(t *testing.T) {
	t.Parallel()

	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f1 := parseFixture(t, name)
			b1, _, err := WriteBytes(f1, WriteOptions{KeepIndices: true})
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			f2, err := ParseBytes(b1)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}

			if len(f1.Cues) != len(f2.Cues) {
				t.Fatalf("cue count changed: %d -> %d", len(f1.Cues), len(f2.Cues))
			}
			for i := range f1.Cues {
				a, b := f1.Cues[i], f2.Cues[i]
				if a.Start != b.Start || a.End != b.End {
					t.Errorf("cue %d timings changed: %v-%v -> %v-%v", i+1, a.Start, a.End, b.Start, b.End)
				}
				if !reflect.DeepEqual(a.Lines, b.Lines) {
					t.Errorf("cue %d text changed:\n got %q\nwant %q", i+1, b.Lines, a.Lines)
				}
				// An index-less cue picks up its sequence number;
				// everything else must be verbatim.
				if a.Index != "" && a.Index != b.Index {
					t.Errorf("cue %d index changed: %q -> %q", i+1, a.Index, b.Index)
				}
			}
			if !reflect.DeepEqual(f1.BOM, f2.BOM) {
				t.Errorf("BOM changed: %v -> %v", f1.BOM, f2.BOM)
			}
			if f1.LineEnding != Mixed && f1.LineEnding != f2.LineEnding {
				t.Errorf("line ending changed: %v -> %v", f1.LineEnding, f2.LineEnding)
			}
			if want := max(f1.TrailingNewlines, 1); len(f1.Cues) > 0 && f2.TrailingNewlines != want {
				t.Errorf("trailing newlines = %d, want %d", f2.TrailingNewlines, want)
			}
		})
	}
}

// The documented normalisation: blank lines at the end of a cue's text and runs
// of blank lines between cues collapse. It happens once, on the first write,
// and never again.
func TestBlankLineNormalisationIsIdempotent(t *testing.T) {
	t.Parallel()

	const in = "1\n00:00:01,000 --> 00:00:02,000\ntext\n\n\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nkeeps\n\nan interior blank\n\n\n"
	const want = "1\n00:00:01,000 --> 00:00:02,000\ntext\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nkeeps\n\nan interior blank\n\n\n"

	f, err := ParseBytes([]byte(in))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := len(f.Cues[1].Lines); got != 3 {
		t.Errorf("interior blank line was dropped: %q", f.Cues[1].Lines)
	}

	b, _, err := WriteBytes(f, WriteOptions{KeepIndices: true})
	if err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if string(b) != want {
		t.Fatalf("normalised output:\n got %q\nwant %q", b, want)
	}

	f2, err := ParseBytes(b)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	b2, _, err := WriteBytes(f2, WriteOptions{KeepIndices: true})
	if err != nil {
		t.Fatalf("second WriteBytes: %v", err)
	}
	if string(b2) != string(b) {
		t.Errorf("normalisation is not idempotent:\n got %q\nwant %q", b2, b)
	}
}
