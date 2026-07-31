package mkv

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	"unicode/utf8"
)

// defaultTimestampScale is Matroska's specified default: one tick is one
// millisecond. Nearly every file states it explicitly anyway.
const defaultTimestampScale = 1_000_000 * time.Nanosecond

// maxTimestampScale bounds TimestampScale at one second per tick. The
// specification sets no ceiling, but the bound is what makes the tick
// arithmetic provably overflow-free, and a file needing coarser ticks than one
// second cannot express a subtitle timing anyway.
const maxTimestampScale = time.Second

// Inference bounds for a cue whose block carried no BlockDuration.
//
// maxInferredDuration is what stops a cue before a long silence from being held
// on screen for the whole of it: seven seconds is above the longest cue in a
// normally authored subtitle and well below the gap between scenes.
// fallbackDuration is for the last cue of a file, which has no successor to
// take a time from.
const (
	maxInferredDuration = 7 * time.Second
	fallbackDuration    = 2 * time.Second
)

// ErrNotMatroska reports a file that is not a Matroska container at all. It is
// separate from a parse failure because the two need different advice: one is a
// wrong file, the other is a broken one.
var ErrNotMatroska = errors.New("not a Matroska file")

// ErrNoSuchTrack reports a request for a track number the file does not have.
var ErrNoSuchTrack = errors.New("no such track")

// Reader is an open Matroska file with its header parsed.
//
// Creating one reads the EBML header, Info and Tracks and nothing else, so
// listing the tracks of a 1 GB film is instant. [Reader.Cues] is what walks the
// clusters.
type Reader struct {
	r          *reader
	docType    string
	scale      time.Duration
	tracks     []Track
	segmentEnd int64

	// clustersFrom is where the first Cluster's ID byte sits, so that Cues
	// can resume the segment walk there instead of from the top. It is -1
	// when the file has no clusters, which is different from 0.
	clustersFrom int64

	warnings []string
}

// NewReader parses the header of a Matroska file. rs must be seekable; a
// subtitle track is spread across the whole container, so there is no reading
// it out of a pipe.
func NewReader(rs io.ReadSeeker) (*Reader, error) {
	m := &Reader{
		r:            newReader(rs),
		scale:        defaultTimestampScale,
		clustersFrom: -1,
	}
	if err := m.openSegment(); err != nil {
		return nil, err
	}
	if err := m.scanSegment(); err != nil {
		return nil, err
	}
	return m, nil
}

// DocType is the file's EBML DocType, "matroska" or "webm".
func (m *Reader) DocType() string { return m.docType }

// TimestampScale is the duration of one timestamp tick.
func (m *Reader) TimestampScale() time.Duration { return m.scale }

// Tracks are every track the file declares, in file order.
func (m *Reader) Tracks() []Track { return m.tracks }

// Warnings are diagnostics raised while reading the header.
func (m *Reader) Warnings() []string { return m.warnings }

// SubtitleTracks are the subtitle tracks only, in file order.
func (m *Reader) SubtitleTracks() []Track {
	var out []Track
	for _, t := range m.tracks {
		if t.Type == TrackSubtitle {
			out = append(out, t)
		}
	}
	return out
}

// Track looks a track up by its TrackNumber.
func (m *Reader) Track(number uint64) (Track, bool) {
	for _, t := range m.tracks {
		if t.Number == number {
			return t, true
		}
	}
	return Track{}, false
}

// openSegment walks the top-level elements until the Segment is found,
// reading the EBML header on the way past.
//
// The Segment is the one element allowed to declare an unknown size — a file
// muxed as it was recorded cannot know its own length — so it is handled here
// rather than by the generic child walk, which refuses unknown sizes.
func (m *Reader) openSegment() error {
	first := true
	for {
		id, err := m.r.readID()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: no Segment element", ErrNotMatroska)
			}
			// The first four bytes of an .mp4 are a length, so they
			// start with a zero byte and are not a valid element ID at
			// all. That is a wrong file, not a corrupt one, and the
			// difference is the whole of the advice.
			if first {
				return fmt.Errorf("%w: it does not start with an EBML header", ErrNotMatroska)
			}
			return err
		}
		if first && id != idEBML {
			return fmt.Errorf("%w: it does not start with an EBML header", ErrNotMatroska)
		}
		first = false

		size, err := m.r.readSize()
		if err != nil {
			return err
		}
		if id == idSegment {
			if size == sizeUnknown {
				m.segmentEnd = math.MaxInt64
			} else {
				m.segmentEnd = m.r.pos + int64(size)
			}
			return nil
		}
		if size == sizeUnknown {
			return fmt.Errorf("%w: top-level element %#x has an unknown size", ErrNotMatroska, id)
		}
		end := m.r.pos + int64(size)
		if id == idEBML {
			if err := m.readEBMLHeader(end); err != nil {
				return err
			}
		}
		if err := m.r.seekTo(end); err != nil {
			return err
		}
	}
}

// readEBMLHeader reads the DocType and refuses anything that is not Matroska.
//
// An .mp4 or an .avi would otherwise parse a long way into nonsense before
// failing on something unrecognisable, and report that instead of the one fact
// the user needs: this is the wrong kind of file.
func (m *Reader) readEBMLHeader(end int64) error {
	err := m.r.children(end, func(e element) error {
		if e.ID != idDocType {
			return nil
		}
		s, err := m.r.stringValue(e.Size())
		if err != nil {
			return err
		}
		m.docType = s
		return nil
	})
	if err != nil {
		return err
	}
	switch m.docType {
	case "matroska", "webm", "":
		// An absent DocType defaults to "matroska" per the spec.
		return nil
	default:
		return fmt.Errorf("%w: EBML DocType is %q", ErrNotMatroska, m.docType)
	}
}

// scanSegment reads Info and Tracks and finds where the clusters begin.
//
// It does not stop at the first cluster: Tracks legitimately sits after the
// clusters in a file muxed in one pass and indexed afterwards, and a reader
// that gave up early would report such a file as having no tracks at all.
// Stepping over a cluster costs one seek, so the scan is cheap either way.
func (m *Reader) scanSegment() error {
	var haveInfo, haveTracks bool
	err := m.r.children(m.segmentEnd, func(e element) error {
		switch e.ID {
		case idInfo:
			haveInfo = true
			return m.readInfo(e.End)
		case idTracks:
			haveTracks = true
			return m.readTracks(e.End)
		case idCluster:
			if m.clustersFrom < 0 {
				m.clustersFrom = e.Start
			}
		}
		if haveInfo && haveTracks && m.clustersFrom >= 0 {
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		// A truncated container still has usable tracks if the header
		// survived, and the header is all that has been read so far.
		if errors.Is(err, errTruncated) && haveTracks {
			m.warnings = append(m.warnings, fmt.Sprintf("%v; reading what is there", err))
			return nil
		}
		return err
	}
	if !haveTracks {
		return fmt.Errorf("%w: no Tracks element", ErrNotMatroska)
	}
	return nil
}

// errStopWalk ends a child walk early. It never escapes the package.
var errStopWalk = errors.New("stop")

func (m *Reader) readInfo(end int64) error {
	return m.r.children(end, func(e element) error {
		if e.ID != idTimestampScale {
			return nil
		}
		v, err := m.r.uintValue(e.Size())
		if err != nil {
			return err
		}
		scale := time.Duration(v) * time.Nanosecond
		if v == 0 || scale > maxTimestampScale {
			return fmt.Errorf("TimestampScale is %d ns, which is not a usable tick", v)
		}
		m.scale = scale
		return nil
	})
}

func (m *Reader) readTracks(end int64) error {
	return m.r.children(end, func(e element) error {
		if e.ID != idTrackEntry {
			return nil
		}
		t, err := m.readTrackEntry(e.End)
		if err != nil {
			return err
		}
		m.tracks = append(m.tracks, t)
		return nil
	})
}

func (m *Reader) readTrackEntry(end int64) (Track, error) {
	// Two specified defaults have to be applied before reading anything,
	// because a muxer omits an element whose value is already the default.
	//
	// FlagDefault is the one that bites: its default is *1*, so a track with
	// no FlagDefault element is the default track, and ffmpeg writes the
	// element only on the tracks that are not. Initialising it to false
	// reads every track in the file as non-default — quietly, since nothing
	// is missing and nothing fails.
	//
	// Language defaults to "eng", recorded as an assumption rather than as a
	// fact: untagged tracks are common, the default is often wrong, and it
	// ends up in a filename.
	t := Track{Default: true, Language: "eng", LanguageIsDefault: true}
	var bcp47 string

	err := m.r.children(end, func(e element) error {
		var err error
		switch e.ID {
		case idTrackNumber:
			t.Number, err = m.r.uintValue(e.Size())
		case idTrackUID:
			t.UID, err = m.r.uintValue(e.Size())
		case idTrackType:
			var v uint64
			v, err = m.r.uintValue(e.Size())
			t.Type = TrackType(v)
		case idCodecID:
			t.CodecID, err = m.r.stringValue(e.Size())
		case idLanguage:
			var s string
			if s, err = m.r.stringValue(e.Size()); err == nil && s != "" {
				t.Language, t.LanguageIsDefault = s, false
			}
		case idLanguageBCP47:
			bcp47, err = m.r.stringValue(e.Size())
		case idTrackName:
			t.Name, err = m.r.stringValue(e.Size())
		case idFlagDefault:
			t.Default, err = m.r.boolValue(e.Size())
		case idFlagForced:
			t.Forced, err = m.r.boolValue(e.Size())
		case idFlagHearingImpaired:
			t.HearingImpaired, err = m.r.boolValue(e.Size())
		}
		return err
	})
	if err != nil {
		return Track{}, err
	}
	// LanguageBCP47 wins where both are present: the specification says the
	// older element exists only for compatibility, and the two disagree
	// exactly where the distinction matters (pt vs pt-BR, zh vs zh-Hant).
	if bcp47 != "" {
		t.Language, t.LanguageIsDefault = bcp47, false
	}
	return t, nil
}

// Cues reads every cue of one track, in file order.
//
// This is the pass that walks the whole container. Blocks belonging to other
// tracks are stepped over by offset and never read, so the cost is one pass of
// the block headers rather than of the film.
func (m *Reader) Cues(number uint64) (*Result, error) {
	track, ok := m.Track(number)
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrNoSuchTrack, number)
	}
	res := &Result{Track: track, Warnings: m.warnings}

	if m.clustersFrom < 0 {
		res.Warnings = append(res.Warnings, "the file has no clusters, so it carries no subtitle data")
		return res, nil
	}
	if err := m.r.seekTo(m.clustersFrom); err != nil {
		return nil, err
	}

	err := m.r.children(m.segmentEnd, func(e element) error {
		if e.ID != idCluster {
			return nil
		}
		return m.readCluster(e.End, number, res)
	})
	if err != nil {
		// Keep what was read. A container truncated by an interrupted
		// download or a full disk still holds most of its subtitle, and
		// throwing it away to report a tidier error helps nobody — the
		// same reasoning as keeping a partial translation.
		if !errors.Is(err, errTruncated) || len(res.Cues) == 0 {
			return nil, err
		}
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("%v; kept the %d cues that were readable", err, len(res.Cues)))
	}

	finishCues(res)
	return res, nil
}

func (m *Reader) readCluster(end int64, track uint64, res *Result) error {
	var clusterTS int64
	haveTS := false

	return m.r.children(end, func(e element) error {
		switch e.ID {
		case idClusterTime:
			v, err := m.r.uintValue(e.Size())
			if err != nil {
				return err
			}
			if v > math.MaxInt64 {
				return fmt.Errorf("cluster at byte %d has an impossible timestamp", e.Start)
			}
			clusterTS, haveTS = int64(v), true
		case idSimpleBlock:
			// A SimpleBlock has nowhere to put a duration, so a cue in
			// one has no end time in the file. It is taken as a cue all
			// the same and the end is inferred later.
			return m.readBlockInto(e, track, clusterTS, haveTS, -1, res)
		case idBlockGroup:
			return m.readBlockGroup(e, track, clusterTS, haveTS, res)
		}
		return nil
	})
}

// readBlockGroup reads a Block together with the BlockDuration beside it.
//
// The two are read in one walk and combined afterwards because the
// specification fixes no order between them, and a reader that assumed Block
// came first would drop every duration in a file that wrote them the other way.
func (m *Reader) readBlockGroup(g element, track uint64, clusterTS int64, haveTS bool, res *Result) error {
	var blk element
	haveBlock := false
	duration := int64(-1)

	err := m.r.children(g.End, func(e element) error {
		switch e.ID {
		case idBlock:
			blk, haveBlock = e, true
		case idBlockDuration:
			v, err := m.r.uintValue(e.Size())
			if err != nil {
				return err
			}
			if v > math.MaxInt64 {
				return fmt.Errorf("block group at byte %d has an impossible duration", g.Start)
			}
			duration = int64(v)
		}
		return nil
	})
	if err != nil || !haveBlock {
		return err
	}
	if err := m.r.seekTo(blk.DataStart); err != nil {
		return err
	}
	return m.readBlockInto(blk, track, clusterTS, haveTS, duration, res)
}

// readBlockInto appends the block's cue to res, or does nothing when the block
// belongs to another track. duration is in ticks, or negative when the block
// carried none.
func (m *Reader) readBlockInto(e element, track uint64, clusterTS int64, haveTS bool, duration int64, res *Result) error {
	h, err := m.r.readBlockHeader(e.End)
	if err != nil {
		return err
	}
	if h.track != track {
		return nil
	}
	if h.lacing() != 0 {
		// Lacing packs several frames into one block, sharing a single
		// timestamp between them. There is no sane reading of that for a
		// subtitle and no muxer produces it, so the honest answer is to
		// say what was found rather than to invent a split.
		return fmt.Errorf("block at byte %d uses lacing, which is not supported on a subtitle track", e.Start)
	}
	if !haveTS {
		return fmt.Errorf("block at byte %d comes before its cluster's timestamp", e.Start)
	}
	if h.bodyLen > maxBlockSize {
		return fmt.Errorf("block at byte %d claims %d bytes of text", e.Start, h.bodyLen)
	}
	if len(res.Cues) >= maxCues {
		return fmt.Errorf("track %d has more than %d cues", track, maxCues)
	}

	start, err := m.ticks(clusterTS + int64(h.timecode))
	if err != nil {
		return fmt.Errorf("block at byte %d: %w", e.Start, err)
	}
	body, err := m.r.readFull(h.bodyLen)
	if err != nil {
		return err
	}

	c := Cue{Start: start, End: start, Text: string(body)}
	if duration < 0 {
		c.InferredEnd = true
	} else {
		d, err := m.ticks(duration)
		if err != nil {
			return fmt.Errorf("block at byte %d: %w", e.Start, err)
		}
		c.End = start + d
	}
	res.Cues = append(res.Cues, c)
	return nil
}

// ticks converts a tick count to a duration, refusing one large enough to
// overflow. TimestampScale is bounded at one second, so the check is exact.
func (m *Reader) ticks(n int64) (time.Duration, error) {
	scale := int64(m.scale)
	if n > math.MaxInt64/scale || n < math.MinInt64/scale {
		return 0, fmt.Errorf("timestamp %d ticks is out of range", n)
	}
	return time.Duration(n) * m.scale, nil
}

// finishCues fills in inferred end times and reports everything about the
// result that a user would want to know before trusting it.
func finishCues(res *Result) {
	inferred := fillInferredEnds(res.Cues)
	if inferred > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d cues carried no duration; their end times were taken from the cue that follows",
			inferred))
	}

	outOfOrder := 0
	for i := 1; i < len(res.Cues); i++ {
		if res.Cues[i].Start < res.Cues[i-1].Start {
			outOfOrder++
		}
	}
	if outOfOrder > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d cues start before the cue in front of them; file order was kept, not timestamp order",
			outOfOrder))
	}

	if res.Track.Text() {
		bad := 0
		for _, c := range res.Cues {
			if !utf8.ValidString(c.Text) {
				bad++
			}
		}
		if bad > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%d cues are not valid UTF-8 despite the track declaring %s; their bytes were kept as they are",
				bad, res.Track.CodecID))
		}
	}
}

// fillInferredEnds gives an end to every cue whose block had no duration, and
// reports how many there were.
func fillInferredEnds(cues []Cue) int {
	n := 0
	for i := range cues {
		c := &cues[i]
		if !c.InferredEnd {
			continue
		}
		n++
		c.End = c.Start + fallbackDuration
		if i+1 < len(cues) && cues[i+1].Start > c.Start {
			c.End = min(cues[i+1].Start, c.Start+maxInferredDuration)
		}
	}
	return n
}
