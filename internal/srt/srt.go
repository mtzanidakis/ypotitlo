// Package srt implements a deliberately lossless reader and writer for SubRip
// (.srt) subtitle files.
//
// The guiding principle is that a subtitle translation is a round trip: every
// byte that is not translated must come back unchanged. Every silent
// transformation — escaping, trimming, tag normalisation, re-sorting — is data
// loss. Therefore this package:
//
//   - treats [Cue.Lines] as fully opaque. Text is never trimmed, never
//     unescaped, never parsed for markup. "<i>a</i> <i>b</i>", a bare "&",
//     "{\an8}" and four leading spaces all survive a round trip byte for byte.
//   - never sorts, never de-duplicates and never drops cues. Out-of-order and
//     overlapping timings are preserved in file order; empty cues are kept with
//     a non-nil, zero-length Lines slice.
//   - keeps the raw index token as a string ([Cue.Index]), so "007", "0" and a
//     duplicated index can be written back verbatim. Diffing a source file
//     against its translation is how a human checks the work, and renumbering a
//     0-based or gapped file turns every cue into a diff hunk.
//   - records what the input looked like (BOM, line ending, trailing newline
//     count) so the writer can reproduce it instead of guessing.
//
// # Reader
//
// [Parse] buffers all lines first, splitting on "\r\n", "\n" or a lone "\r" —
// CR-only files are real and would otherwise parse as one gigantic line, that
// is, zero cues — and then runs an explicit state machine with two lines of
// lookahead. It is not a paraphrase of ffmpeg's srt_read_header, which loses a
// cue's last text line when that line is numeric and the following cue has no
// index (reproduced with ffmpeg 8.1.2), drops empty cues, and re-sorts by
// timestamp.
//
// A line is an index if and only if all three of these hold: it matches ^\d+$
// on the untrimmed line (so " 555" and "-5" are not indices), the line
// immediately after it parses as a timing line, and the parser is between cues.
// The third condition is what makes the numeric-last-line case decidable.
//
// # Timing grammar
//
// Timing fields are tokenised into digit runs separated by ':', ',' or '.' and
// then disambiguated by which separators were used, never by a single regexp:
//
//  1. a ',' or '.' marks the millisecond boundary. The fields before it are, from
//     right to left, SS, MM, HH; an absent field is zero. Exactly one field may
//     follow it.
//  2. if every separator is ':', four fields mean HH:MM:SS:mmm, three fields mean
//     HH:MM:SS with ms = 0, and two fields mean MM:SS with ms = 0.
//  3. anything else is a parse error naming the line number.
//
// Rule 2 is the important one: accepting "[HH:]MM:SS[:,.]mmm" with an optional
// hour would parse "0:01:30 --> 0:01:35" as 1.030s → 1.035s instead of 90s →
// 95s, landing every cue of the file in its first second with a duration of
// five milliseconds, silently.
//
// Two deliberate deviations from ffmpeg:
//
//   - a millisecond field of one or two digits is a decimal fraction and is
//     left-padded: ",5" is 500ms and ",05" is 50ms. ffmpeg reads ",5" as 5ms.
//   - minute and second fields of 60 or more are kept as written and reported as
//     a warning rather than being rejected.
//
// # Writer
//
// [Write] emits HH:MM:SS,mmm with at least two hour digits, renumbers indices
// from 1 unless [WriteOptions.KeepIndices] is set, and performs zero escaping.
// The BOM and line ending are inherited from the parsed file unless overridden.
//
// # Known limitation
//
// Blank lines inside a cue are normalised. Interior blank lines are preserved as
// empty text lines, but blank lines at the very end of a cue's text are
// indistinguishable from the cue separator and collapse into it, and a run of
// several blank lines between two cues collapses to one. The round trip is
// therefore byte-exact modulo blank-line normalisation, and that normalisation
// is idempotent: parsing and writing a second time reproduces the same bytes.
// TestFixturesAreIdempotent proves this for every fixture.
package srt

import (
	"strconv"
	"time"
)

// utf8BOM is the only BOM this package handles; decoding UTF-16/UTF-32 is the
// job of internal/charset, which strips their BOMs before the text gets here.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// LineEnding is the line terminator convention of a file. A bool cannot express
// Mixed, and mixed-ending files are common enough (hand-edited rips, files
// concatenated by a shell script) that silently picking one is data loss.
type LineEnding int

// The known line terminator conventions. The zero value is LF.
const (
	LF LineEnding = iota
	CRLF
	CR
	Mixed
)

// lineEndingBytes maps the concrete endings to their bytes. Mixed has no bytes
// of its own; the writer resolves it to the majority first.
var lineEndingBytes = [...]string{LF: "\n", CRLF: "\r\n", CR: "\r"}

// String implements fmt.Stringer.
func (e LineEnding) String() string {
	switch e {
	case LF:
		return "LF"
	case CRLF:
		return "CRLF"
	case CR:
		return "CR"
	case Mixed:
		return "Mixed"
	default:
		return "LineEnding(" + strconv.Itoa(int(e)) + ")"
	}
}

// Cue is one subtitle entry.
type Cue struct {
	// Index is the raw index token exactly as it appeared in the input:
	// "007", "0", a duplicate, or "" when the cue had no index line. The
	// reader only ever produces a token matching ^\d+$, but the field is a
	// string so that anything a caller puts here is written back verbatim
	// under WriteOptions.KeepIndices.
	Index string

	// Start and End are the cue timings. They are always millisecond
	// granularity when produced by the reader.
	Start time.Duration
	End   time.Duration

	// Lines is the cue text, one entry per line, without line terminators.
	// It is completely opaque: leading and trailing whitespace, markup,
	// bare ampersands and angle brackets are preserved exactly. It is
	// non-nil and zero-length for a cue with no text.
	Lines []string

	// RecoveredBoundary reports that this cue's boundary had to be inferred
	// because the blank line before it was missing. The corresponding
	// warning is in File.Warnings.
	RecoveredBoundary bool
}

// File is a parsed SRT file together with everything needed to write it back
// out the way it came in.
type File struct {
	Cues []Cue

	// BOM holds the actual BOM bytes found on the input, or nil if there
	// was none. It is bytes rather than a bool so that the exact BOM can be
	// re-emitted.
	BOM []byte

	// LineEnding is the terminator convention detected on input.
	LineEnding LineEnding

	// TrailingNewlines is the exact number of line terminators at the end
	// of the input: 2 for the usual "text\n\n", 0 for a file whose last
	// line is unterminated.
	TrailingNewlines int

	// Warnings holds human-readable diagnostics, each prefixed with the
	// input line number where that is meaningful. Warnings never mean data
	// was dropped; they mean something was guessed or is unusual.
	Warnings []string

	// lineEndingCounts is how many of each concrete terminator the input
	// used, indexed by LineEnding. The writer needs it to resolve Mixed to
	// the majority; a hand-built File has none and resolves to LF.
	lineEndingCounts [3]int
}

// majorityLineEnding resolves LineEnding to a terminator that can actually be
// written. Mixed becomes whichever concrete ending appeared most often, LF on a
// tie or on a hand-built File.
func (f *File) majorityLineEnding() LineEnding {
	switch f.LineEnding {
	case LF, CRLF, CR:
		return f.LineEnding
	case Mixed:
		best, bestN := LF, 0
		for _, e := range []LineEnding{LF, CRLF, CR} {
			if n := f.lineEndingCounts[e]; n > bestN {
				best, bestN = e, n
			}
		}
		return best
	default:
		return LF
	}
}
