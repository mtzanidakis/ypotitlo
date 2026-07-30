package srt

import (
	"bytes"
	"fmt"
	"io"
	"time"
)

// Parse reads an SRT file from r. The input must already be UTF-8 text;
// decoding legacy encodings is the job of internal/charset.
func Parse(r io.Reader) (*File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("srt: read: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes reads an SRT file from data. An empty input is not an error: it
// yields a File with no cues.
func ParseBytes(data []byte) (*File, error) {
	f := &File{}
	if bytes.HasPrefix(data, utf8BOM) {
		f.BOM = append([]byte(nil), utf8BOM...)
		data = data[len(utf8BOM):]
	}

	lines, counts, trailing := splitLines(string(data))
	f.lineEndingCounts = counts
	f.TrailingNewlines = trailing
	f.LineEnding = classifyLineEnding(counts)
	if f.LineEnding == Mixed {
		f.Warnings = append(f.Warnings, fmt.Sprintf(
			"mixed line endings: %d LF, %d CRLF, %d CR", counts[LF], counts[CRLF], counts[CR]))
	}

	p := parser{lines: lines, file: f}
	if err := p.run(); err != nil {
		return nil, err
	}
	return f, nil
}

// splitLines splits s on "\r\n", "\n" or a lone "\r", returning the lines
// without their terminators, how many of each terminator was used, and the
// exact number of terminators at the end of the input.
//
// The lone "\r" case matters: a CR-only file split on "\n" alone becomes a
// single enormous line, which parses as zero cues.
func splitLines(s string) (lines []string, counts [3]int, trailing int) {
	start := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '\n':
			lines = append(lines, s[start:i])
			counts[LF]++
			i++
		case '\r':
			lines = append(lines, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				counts[CRLF]++
				i += 2
			} else {
				counts[CR]++
				i++
			}
		default:
			i++
			continue
		}
		start = i
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}

	for i := len(s); i > 0; trailing++ {
		switch {
		case s[i-1] == '\n' && i >= 2 && s[i-2] == '\r':
			i -= 2
		case s[i-1] == '\n' || s[i-1] == '\r':
			i--
		default:
			return lines, counts, trailing
		}
	}
	return lines, counts, trailing
}

// classifyLineEnding reports the single terminator convention used, or Mixed
// when more than one appears. An input with no terminator at all is LF, the
// default.
func classifyLineEnding(counts [3]int) LineEnding {
	found, kinds := LF, 0
	for _, e := range []LineEnding{LF, CRLF, CR} {
		if counts[e] > 0 {
			found = e
			kinds++
		}
	}
	if kinds > 1 {
		return Mixed
	}
	return found
}

// isDigits reports whether line matches ^\d+$ on the untrimmed line. " 555"
// and "-5" are deliberately not indices.
func isDigits(line string) bool {
	if line == "" {
		return false
	}
	for i := 0; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			return false
		}
	}
	return true
}

// parser is the reader state machine. Every line has already been buffered, so
// the two-line lookahead needed to decide whether a numeric line is an index is
// just an array index and no cache is required.
type parser struct {
	lines []string
	file  *File
	cur   *Cue
}

func (p *parser) run() error {
	// between is the "between cues" state: at the start of the file, right
	// after a blank line, or right after a cue was closed. It is condition
	// (c) of the index rule, the condition ffmpeg lacks.
	between := true
	// pendingBlanks counts blank lines that have not yet been resolved into
	// either a cue separator or interior blank text lines.
	pendingBlanks := 0

	for i := 0; i < len(p.lines); {
		line := p.lines[i]

		if line == "" {
			pendingBlanks++
			between = true
			i++
			continue
		}

		if between {
			n, ok, err := p.startAtBoundary(i)
			if err != nil {
				return err
			}
			if ok {
				pendingBlanks, between = 0, false
				i += n
				continue
			}
		} else if n, ok := p.startAtRecovery(i); ok {
			pendingBlanks = 0
			i += n
			continue
		}

		if p.cur == nil {
			return p.noCueError(i)
		}
		// Interior blank lines are kept as empty text lines. Blank lines
		// at the end of a cue's text are indistinguishable from the cue
		// separator and are the documented normalisation.
		for ; pendingBlanks > 0; pendingBlanks-- {
			p.cur.Lines = append(p.cur.Lines, "")
		}
		p.cur.Lines = append(p.cur.Lines, line)
		between = false
		i++
	}

	p.closeCue()
	return nil
}

// startAtBoundary tries to open a cue at an explicit boundary: the parser is
// between cues, so the line may be an index followed by a timing line, or a
// bare timing line. It returns how many lines were consumed.
func (p *parser) startAtBoundary(i int) (consumed int, ok bool, err error) {
	// blank + ^\d+$ + timing
	if isDigits(p.lines[i]) && i+1 < len(p.lines) {
		if start, end, warns, tsErr := parseTimeLine(p.lines[i+1]); tsErr == nil {
			p.startCue(p.lines[i], start, end, false, i+1, warns)
			return 2, true, nil
		}
	}

	// blank + timing. An index-less cue is unusual but unambiguous when the
	// boundary itself is explicit, so it is not a recovered boundary.
	start, end, warns, tsErr := parseTimeLine(p.lines[i])
	if tsErr == nil {
		p.startCue("", start, end, false, i, warns)
		return 1, true, nil
	}

	// A line that looks like a timing line but does not parse is almost
	// always a corrupt file rather than cue text. With no cue open there is
	// nowhere to put it, so say precisely what was wrong and where;
	// otherwise keep it as text and warn.
	if hasArrow(p.lines[i]) {
		if p.cur == nil {
			return 0, false, fmt.Errorf("srt: line %d: %w", i+1, tsErr)
		}
		p.warnf(i, "line that looks like a timing line kept as cue text: %v", tsErr)
	}
	return 0, false, nil
}

// startAtRecovery handles a missing blank separator line:
//
//	First cue.
//	2
//	00:00:03,000 --> 00:00:04,000
//
// The timing line is accepted as a boundary only if the current cue already has
// at least one text line and the line immediately before was consumed as an
// index, which is exactly what keeps
//
//	Call me at
//	555
//
//	00:00:03,000 --> 00:00:04,000
//
// from losing "555": there the previous line is blank, the ordinary boundary
// rule fires, and the numeric line stays where it belongs. ffmpeg drops it.
func (p *parser) startAtRecovery(i int) (consumed int, ok bool) {
	start, end, warns, err := parseTimeLine(p.lines[i])
	if err != nil {
		return 0, false
	}
	if p.cur == nil || len(p.cur.Lines) == 0 || i == 0 || !isDigits(p.lines[i-1]) {
		return 0, false
	}

	// The previous line was appended as text one iteration ago; take it
	// back and use it as the new cue's index.
	index := p.cur.Lines[len(p.cur.Lines)-1]
	p.cur.Lines = p.cur.Lines[:len(p.cur.Lines)-1]
	// The warning names the index line, which is where the missing blank
	// line belongs.
	p.warnf(i-1, "missing blank line before cue %q; boundary recovered", index)
	p.startCue(index, start, end, true, i, warns)
	return 1, true
}

// noCueError explains why line i cannot be the start of a cue. When the line
// after it looks like a broken timing line, that is almost always the real
// fault — an index whose timing line does not parse — so name that line
// instead of the index above it.
func (p *parser) noCueError(i int) error {
	if i+1 < len(p.lines) && hasArrow(p.lines[i+1]) {
		if _, _, _, err := parseTimeLine(p.lines[i+1]); err != nil {
			return fmt.Errorf("srt: line %d: %w", i+2, err)
		}
	}
	return fmt.Errorf("srt: line %d: expected a cue index or timing line, got %q", i+1, p.lines[i])
}

func (p *parser) startCue(index string, start, end time.Duration, recovered bool, timeLine int, warns []string) {
	p.closeCue()
	p.cur = &Cue{
		Index:             index,
		Start:             start,
		End:               end,
		Lines:             []string{},
		RecoveredBoundary: recovered,
	}
	for _, w := range warns {
		p.warnf(timeLine, "%s", w)
	}
}

func (p *parser) closeCue() {
	if p.cur == nil {
		return
	}
	p.file.Cues = append(p.file.Cues, *p.cur)
	p.cur = nil
}

// warnf appends a warning prefixed with the 1-based number of line i.
func (p *parser) warnf(i int, format string, a ...any) {
	p.file.Warnings = append(p.file.Warnings, fmt.Sprintf("line %d: %s", i+1, fmt.Sprintf(format, a...)))
}
