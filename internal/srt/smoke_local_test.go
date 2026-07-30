package srt

import (
	"bytes"
	"os"
	"testing"
)

// TestLocalSample parses the working-copy sample subtitle when present. The
// file is gitignored, so this skips in CI and on a fresh clone.
func TestLocalSample(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../sirat.en.srt")
	if err != nil {
		t.Skip("no local sample")
	}

	f, err := ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(f.Cues), 656; got != want {
		t.Errorf("cues = %d, want %d", got, want)
	}
	if f.LineEnding != LF {
		t.Errorf("line ending = %v, want LF", f.LineEnding)
	}
	if f.BOM != nil {
		t.Errorf("BOM = %x, want none", f.BOM)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", f.Warnings)
	}

	out, _, err := WriteBytes(f, WriteOptions{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(raw, out) {
		t.Errorf("round trip differs: in %d bytes, out %d bytes", len(raw), len(out))
	}
}
