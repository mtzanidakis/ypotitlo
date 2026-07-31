// Package mkv reads the subtitle tracks of a Matroska container (.mkv, .mka,
// .webm) without decoding anything else in it.
//
// It exists rather than shelling out to ffmpeg for the same reason
// [github.com/mtzanidakis/ypotitlo/internal/srt] exists rather than importing
// an SRT library: a subtitle that passes through a general-purpose media tool
// comes out slightly different, and none of the differences produce an error.
// Here the cue text is the bytes of the Matroska block, copied out and never
// interpreted, and a timing is the block's own timestamp arithmetic. There is
// no external binary to install, to version-skew against, or to be absent on a
// user's machine.
//
// # Scope
//
// Only what a subtitle extractor needs is parsed: the EBML header, Info (for
// TimestampScale), Tracks, and the blocks of one requested track. Video and
// audio blocks are seeked past without ever being read into memory, so a 1 GB
// film costs one pass of the block headers rather than one pass of the film.
//
// # Timings
//
// A cue's start is (Cluster.Timestamp + Block.Timestamp) × TimestampScale, and
// its end is that plus BlockDuration × TimestampScale. TrackTimestampScale is
// deliberately ignored: it is deprecated, defaults to 1.0, and every muxer that
// still writes it writes 1.0.
//
// A SimpleBlock cannot carry a duration, so a subtitle muxed into one has no
// end time in the file at all. Such cues are given the next cue's start and
// reported in [Result.Warnings], because a guessed duration that says it was
// guessed is more useful than a refusal — and far more useful than a silent
// two-second default.
//
// # What is not handled
//
// Laced blocks are refused rather than guessed at. Lacing packs several frames
// into one block with no timestamp of their own, which is meaningless for
// subtitles and is not emitted by any muxer in circulation; writing untested
// code to split them would be a worse answer than an error that names the
// problem.
//
// Cues are returned in file order, never sorted. Matroska clusters are ordered
// by timestamp so this is nearly always ascending anyway, and a file where it
// is not gets a warning rather than a quiet reshuffle.
package mkv

import (
	"fmt"
	"time"
)

// TrackType is the Matroska TrackType enumeration.
type TrackType uint64

// The track types. Only TrackSubtitle is of any interest here, but the others
// are named so that a listing can say what it skipped.
const (
	TrackVideo    TrackType = 0x01
	TrackAudio    TrackType = 0x02
	TrackComplex  TrackType = 0x03
	TrackLogo     TrackType = 0x10
	TrackSubtitle TrackType = 0x11
	TrackButtons  TrackType = 0x12
	TrackControl  TrackType = 0x20
	TrackMetadata TrackType = 0x21
)

// String implements fmt.Stringer.
func (t TrackType) String() string {
	switch t {
	case TrackVideo:
		return "video"
	case TrackAudio:
		return "audio"
	case TrackComplex:
		return "complex"
	case TrackLogo:
		return "logo"
	case TrackSubtitle:
		return "subtitle"
	case TrackButtons:
		return "buttons"
	case TrackControl:
		return "control"
	case TrackMetadata:
		return "metadata"
	default:
		return fmt.Sprintf("TrackType(%#x)", uint64(t))
	}
}

// Codec IDs this package can turn into cues. Matroska stores a SubRip track as
// the cue text alone: the index and the timing line are the container's job, so
// what comes out of a block is exactly the text between them.
const (
	CodecUTF8   = "S_TEXT/UTF8"
	CodecASCII  = "S_TEXT/ASCII"
	CodecWebVTT = "S_TEXT/WEBVTT"
	CodecASS    = "S_TEXT/ASS"
	CodecSSA    = "S_TEXT/SSA"
	CodecUSF    = "S_TEXT/USF"
	CodecPGS    = "S_HDMV/PGS"
	CodecTextST = "S_HDMV/TEXTST"
	CodecVobSub = "S_VOBSUB"
	CodecKate   = "S_KATE"
	CodecDVB    = "S_DVBSUB"
)

// Track is one track of a Matroska file, as described by its TrackEntry.
type Track struct {
	// Number is the TrackNumber, which is what blocks refer to. It is not
	// an index into any slice and it is not ffprobe's stream index, which
	// is zero-based; a listing that prints one and accepts the other is a
	// listing that extracts the wrong track.
	Number uint64

	// UID is the TrackUID, stable across remuxes.
	UID uint64

	// Type is the TrackType. Everything but TrackSubtitle is uninteresting
	// here but is still reported, so a file with no subtitles can be told
	// apart from a file that was not read properly.
	Type TrackType

	// CodecID is the raw Matroska codec identifier, "S_TEXT/UTF8" and
	// friends. It is kept verbatim rather than mapped to an enum so that an
	// unknown codec can still be named in an error message.
	CodecID string

	// Language is the track's language exactly as the file tagged it: a
	// BCP-47 tag when the file used LanguageBCP47, otherwise the ISO 639-2
	// code. It is empty when the file said nothing, which callers must not
	// confuse with Matroska's "eng" default — see LanguageIsDefault.
	Language string

	// LanguageIsDefault reports that Language holds Matroska's specified
	// default of "eng" because the file carried no language element at all.
	// A great many rips are tagged this way by accident, so a filename
	// derived from it is a guess and has to say so.
	LanguageIsDefault bool

	// Name is the human-readable track name, often the only place a file
	// records "Forced" or "SDH".
	Name string

	// Default, Forced and HearingImpaired are the corresponding flags.
	Default         bool
	Forced          bool
	HearingImpaired bool
}

// Text reports whether the track's cue payload is text this package turns
// straight into cues.
func (t Track) Text() bool {
	return t.CodecID == CodecUTF8 || t.CodecID == CodecASCII
}

// CodecName describes the codec in words, for a listing. An unknown codec is
// returned as its own ID rather than as "unknown", because the ID is the thing
// a user can search for.
func (t Track) CodecName() string {
	switch t.CodecID {
	case CodecUTF8:
		return "SubRip text"
	case CodecASCII:
		return "plain text"
	case CodecWebVTT:
		return "WebVTT"
	case CodecASS:
		return "ASS"
	case CodecSSA:
		return "SSA"
	case CodecUSF:
		return "USF"
	case CodecPGS:
		return "PGS bitmap"
	case CodecTextST:
		return "TextST"
	case CodecVobSub:
		return "VobSub bitmap"
	case CodecKate:
		return "Kate"
	case CodecDVB:
		return "DVB bitmap"
	case "":
		return "(none)"
	default:
		return t.CodecID
	}
}

// Cue is one subtitle entry read out of a block.
type Cue struct {
	// Start and End are absolute times from the start of the segment,
	// already scaled by TimestampScale.
	Start, End time.Duration

	// Text is the block payload verbatim, line terminators included. It is
	// not trimmed, not unescaped and not parsed for markup: turning it into
	// lines is the caller's decision, and this package makes none of it.
	Text string

	// InferredEnd reports that the block carried no BlockDuration and End
	// had to be worked out from the following cue.
	InferredEnd bool
}

// Result is everything one extraction produced.
type Result struct {
	// Cues are in file order.
	Cues []Cue

	// Track is the track the cues came from, so a caller that selected it
	// by some rule of its own can report what the rule chose.
	Track Track

	// Warnings are human-readable diagnostics. As in internal/srt, a
	// warning never means data was dropped; it means something was guessed
	// or is unusual.
	Warnings []string
}
