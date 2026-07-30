package srt

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
)

// WriteOptions controls the output. Every pointer field is tri-state: a nil
// pointer means "inherit from the File", which is what preserves the shape of
// the input when the caller has no opinion. A plain bool cannot express that,
// and a caller that cannot express "no opinion" silently overrides the input on
// every run.
type WriteOptions struct {
	// KeepIndices re-emits Cue.Index verbatim instead of renumbering from
	// 1. A cue with an empty Index falls back to its sequence number.
	KeepIndices bool

	// BOM forces a UTF-8 BOM on or off. Nil inherits File.BOM.
	BOM *bool

	// LineEnding forces a line terminator. Nil inherits File.LineEnding,
	// resolving Mixed to the majority.
	LineEnding *LineEnding
}

// Write encodes f to w. It performs no escaping whatsoever: the bytes of
// Cue.Lines go out exactly as they came in.
//
// The returned warnings describe choices the writer had to make; they are
// returned rather than appended to f because writing must not mutate its input.
func Write(w io.Writer, f *File, opts WriteOptions) (warnings []string, err error) {
	eol, warnings := resolveLineEnding(f, opts)
	bw := bufio.NewWriter(w)
	ew := &errWriter{w: bw}

	if bom := resolveBOM(f, opts); len(bom) > 0 {
		ew.Write(bom)
	}

	for i := range f.Cues {
		c := &f.Cues[i]
		if i > 0 {
			ew.WriteString(eol) // the blank line between cues
		}
		ew.WriteString(cueIndex(c, i, opts.KeepIndices))
		ew.WriteString(eol)
		if c.End < c.Start {
			warnings = append(warnings, fmt.Sprintf("cue %d: end is before start", i+1))
		}
		if c.Start < 0 || c.End < 0 {
			warnings = append(warnings, fmt.Sprintf("cue %d: negative timing clamped to zero", i+1))
		}
		ew.WriteString(formatTime(c.Start))
		ew.WriteString(" --> ")
		ew.WriteString(formatTime(c.End))
		ew.WriteString(eol)
		for _, line := range c.Lines {
			ew.WriteString(line)
			ew.WriteString(eol)
		}
	}

	// One terminator has already been written after the last line of the
	// last cue, so only the remainder is left. A file with no cues gets
	// nothing: a lone newline would be an invention.
	if len(f.Cues) > 0 {
		for n := max(f.TrailingNewlines, 1); n > 1; n-- {
			ew.WriteString(eol)
		}
	}

	if ew.err != nil {
		return warnings, fmt.Errorf("srt: write: %w", ew.err)
	}
	if err := bw.Flush(); err != nil {
		return warnings, fmt.Errorf("srt: write: %w", err)
	}
	return warnings, nil
}

// WriteBytes is Write into a new byte slice.
func WriteBytes(f *File, opts WriteOptions) ([]byte, []string, error) {
	var buf bytes.Buffer
	warnings, err := Write(&buf, f, opts)
	return buf.Bytes(), warnings, err
}

// cueIndex picks the index token for the i-th cue.
func cueIndex(c *Cue, i int, keep bool) string {
	if keep && c.Index != "" {
		return c.Index
	}
	return strconv.Itoa(i + 1)
}

func resolveBOM(f *File, opts WriteOptions) []byte {
	if opts.BOM == nil {
		return f.BOM
	}
	if *opts.BOM {
		return utf8BOM
	}
	return nil
}

func resolveLineEnding(f *File, opts WriteOptions) (eol string, warnings []string) {
	e := f.majorityLineEnding()
	if opts.LineEnding != nil {
		e = *opts.LineEnding
		if e == Mixed {
			// Nothing to take a majority of: an explicit request for
			// Mixed is meaningless, so fall back to the file's own.
			e = f.majorityLineEnding()
		}
	} else if f.LineEnding == Mixed {
		warnings = append(warnings, fmt.Sprintf("input has mixed line endings; writing everything with %s", e))
	}
	if e < LF || int(e) >= len(lineEndingBytes) {
		e = LF
	}
	return lineEndingBytes[e], warnings
}

// errWriter defers error handling to the end of a run of writes. Checking every
// individual write of a subtitle file would be all error handling and no logic.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) WriteString(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s)
}

func (e *errWriter) Write(b []byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(b)
}
