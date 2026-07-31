package mkv

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
)

// EBML element IDs, in the canonical form that keeps the length marker, which
// is how the Matroska specification writes them and how they appear in a hex
// dump. Stripping the marker would make 0xA3 (SimpleBlock) and 0x23 collide.
const (
	idEBML     = 0x1A45DFA3
	idSegment  = 0x18538067
	idVoid     = 0xEC
	idCRC32    = 0xBF
	idDocType  = 0x4282
	idEBMLRead = 0x42F7 // EBMLReadVersion

	idInfo           = 0x1549A966
	idTimestampScale = 0x2AD7B1

	idTracks              = 0x1654AE6B
	idTrackEntry          = 0xAE
	idTrackNumber         = 0xD7
	idTrackUID            = 0x73C5
	idTrackType           = 0x83
	idFlagDefault         = 0x88
	idFlagForced          = 0x55AA
	idFlagHearingImpaired = 0x55AB
	idCodecID             = 0x86
	idLanguage            = 0x22B59C
	idLanguageBCP47       = 0x22B59D
	idTrackName           = 0x536E

	idCluster       = 0x1F43B675
	idClusterTime   = 0xE7
	idSimpleBlock   = 0xA3
	idBlockGroup    = 0xA0
	idBlock         = 0xA1
	idBlockDuration = 0x9B
)

// sizeUnknown is the value readSize reports for the all-ones encoding, which
// EBML uses to mean "this element ends when its parent does". It is a sentinel
// rather than an error because a Segment of unknown size is normal in a file
// that was muxed as it was recorded.
const sizeUnknown = ^uint64(0)

// Bounds on what will be read into memory. A corrupt or hostile file can claim
// any size it likes for an element, and a claim is all it takes to allocate:
// the bytes need never exist. Both limits are far above anything real — the
// longest attested subtitle block is a few kilobytes — so hitting one means the
// file is wrong, and saying which limit was hit says where.
const (
	maxStringSize = 64 << 10  // element strings: codec ids, names, languages
	maxBlockSize  = 4 << 20   // one block payload
	maxCues       = 1 << 20   // cues in a single track
	discardLimit  = 512 << 10 // skip by reading rather than seeking below this
)

// errTruncated reports that the file ended inside an element that declared a
// length. It is distinguished from a clean end-of-file because a truncated
// download is worth naming as such.
var errTruncated = errors.New("file ends in the middle of an element")

// reader is a position-tracking reader over a Matroska file.
//
// The position is tracked rather than queried because every element is
// navigated by absolute offset: the parent of an element always knows where
// that element ends, so a child parser that reads too little, too much, or
// nothing at all cannot desynchronise the parse. Seeking to the recorded end
// after each child is what makes an unknown element free to ignore.
type reader struct {
	rs  io.ReadSeeker
	br  *bufio.Reader
	pos int64
}

func newReader(rs io.ReadSeeker) *reader {
	return &reader{rs: rs, br: bufio.NewReaderSize(rs, 64<<10)}
}

// seekTo moves to an absolute offset.
//
// A short forward hop is done by reading, not seeking: the bulk of this
// package's work is stepping over video and audio blocks, and a seek discards
// the read-ahead buffer, so seeking over a 3 KiB block would turn one buffered
// read into one syscall per block. Anything larger is worth the seek, and that
// is what keeps a 1 GB film from being read through end to end.
func (r *reader) seekTo(off int64) error {
	switch {
	case off == r.pos:
		return nil
	case off < r.pos:
		// Only the cluster rescan goes backwards, and only once.
	case off-r.pos <= discardLimit:
		n, err := r.br.Discard(int(off - r.pos))
		r.pos += int64(n)
		if err != nil {
			return r.wrapEOF(err)
		}
		return nil
	}

	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("seek to %d: %w", off, err)
	}
	r.br.Reset(r.rs)
	r.pos = off
	return nil
}

// wrapEOF turns the two end-of-file spellings into one meaningful error.
func (r *reader) wrapEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w at byte %d", errTruncated, r.pos)
	}
	return err
}

// readByte reads one byte. io.EOF is returned unwrapped so that the element
// loop can tell a clean end of file from a truncated element.
func (r *reader) readByte() (byte, error) {
	b, err := r.br.ReadByte()
	if err != nil {
		return 0, err
	}
	r.pos++
	return b, nil
}

// readFull reads exactly n bytes.
func (r *reader) readFull(n int64) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r.br, buf)
	r.pos += int64(got)
	if err != nil {
		return nil, r.wrapEOF(err)
	}
	return buf, nil
}

// readID reads an element ID: one to four bytes, marker included.
func (r *reader) readID() (uint32, error) {
	b0, err := r.readByte()
	if err != nil {
		return 0, err
	}
	if b0 == 0 {
		return 0, fmt.Errorf("invalid element id at byte %d: leading byte is zero", r.pos-1)
	}
	n := bits.LeadingZeros8(b0) + 1
	if n > 4 {
		return 0, fmt.Errorf("invalid element id at byte %d: %d-byte id", r.pos-1, n)
	}
	id := uint32(b0)
	for range n - 1 {
		b, err := r.readByte()
		if err != nil {
			return 0, r.wrapEOF(err)
		}
		id = id<<8 | uint32(b)
	}
	return id, nil
}

// readSize reads an element data size, returning sizeUnknown for the all-ones
// encoding.
func (r *reader) readSize() (uint64, error) {
	b0, err := r.readByte()
	if err != nil {
		return 0, r.wrapEOF(err)
	}
	if b0 == 0 {
		return 0, fmt.Errorf("invalid element size at byte %d: leading byte is zero", r.pos-1)
	}
	n := bits.LeadingZeros8(b0) + 1
	v := uint64(b0) &^ (1 << (8 - n))
	for range n - 1 {
		b, err := r.readByte()
		if err != nil {
			return 0, r.wrapEOF(err)
		}
		v = v<<8 | uint64(b)
	}
	if v == 1<<(7*n)-1 {
		return sizeUnknown, nil
	}
	// An element longer than the address space is a corrupt size, not a
	// large file; catching it here keeps every downstream int64 honest.
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("element size %d at byte %d is impossible", v, r.pos)
	}
	return v, nil
}

// readVint reads a VINT with its length marker stripped: the encoding a Block
// uses for its track number.
func (r *reader) readVint() (uint64, error) {
	v, err := r.readSize()
	if err != nil {
		return 0, err
	}
	if v == sizeUnknown {
		return 0, fmt.Errorf("reserved all-ones vint at byte %d", r.pos)
	}
	return v, nil
}

// element locates one EBML element in the file.
type element struct {
	ID uint32

	// Start is the offset of the element's first ID byte, DataStart the
	// offset of its body, and End the offset just past its body. All three
	// are absolute, which is what lets a child parser be sloppy: the parent
	// always knows where to resume.
	Start     int64
	DataStart int64
	End       int64
}

// Size is the length of the element's body.
func (e element) Size() int64 { return e.End - e.DataStart }

// children walks the child elements of an element whose data ends at end,
// calling fn for each.
//
// fn may read as much or as little of a child's body as it likes: the loop
// seeks to the recorded end afterwards regardless. end is math.MaxInt64 for an
// element of unknown size, in which case a clean end of file ends the walk
// rather than failing it.
func (r *reader) children(end int64, fn func(e element) error) error {
	for r.pos < end {
		start := r.pos
		id, err := r.readID()
		if err != nil {
			if errors.Is(err, io.EOF) && end == math.MaxInt64 {
				return nil
			}
			return r.wrapEOF(err)
		}
		size, err := r.readSize()
		if err != nil {
			return err
		}
		if size == sizeUnknown {
			// Only a Segment may plausibly be of unknown size, and its
			// caller handles that before getting here. Anything else is
			// a live stream this package cannot navigate by offset, and
			// guessing where the element ends would silently drop cues.
			return fmt.Errorf("element %#x at byte %d has an unknown size, which is not supported", id, start)
		}
		e := element{ID: id, Start: start, DataStart: r.pos, End: r.pos + int64(size)}
		if e.End > end {
			return fmt.Errorf("element %#x at byte %d claims %d bytes, past the end of its parent", id, start, size)
		}
		if err := fn(e); err != nil {
			return err
		}
		if err := r.seekTo(e.End); err != nil {
			return err
		}
	}
	return nil
}

// uintValue reads an unsigned integer element body of up to eight bytes.
// A zero-length body is zero, which the specification allows and some muxers
// emit for a flag.
func (r *reader) uintValue(size int64) (uint64, error) {
	if size > 8 {
		return 0, fmt.Errorf("unsigned integer element at byte %d is %d bytes", r.pos, size)
	}
	buf, err := r.readFull(size)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, b := range buf {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// stringValue reads a string element body.
//
// Trailing NUL bytes are stripped: EBML permits an ASCII string to be padded
// with them so it can be rewritten in place, and a language of "eng\x00" would
// otherwise fail to resolve and land in a filename.
func (r *reader) stringValue(size int64) (string, error) {
	if size > maxStringSize {
		return "", fmt.Errorf("string element at byte %d claims %d bytes", r.pos, size)
	}
	buf, err := r.readFull(size)
	if err != nil {
		return "", err
	}
	for len(buf) > 0 && buf[len(buf)-1] == 0 {
		buf = buf[:len(buf)-1]
	}
	return string(buf), nil
}

// boolValue reads a flag element.
func (r *reader) boolValue(size int64) (bool, error) {
	v, err := r.uintValue(size)
	return v != 0, err
}

// blockHeader is the fixed part every Block and SimpleBlock starts with.
type blockHeader struct {
	track    uint64
	timecode int16 // signed, relative to the cluster timestamp
	flags    byte
	bodyLen  int64 // payload bytes remaining after the header
}

// lacing reports the block's lacing mode, 0 meaning none.
func (h blockHeader) lacing() byte { return (h.flags >> 1) & 0x03 }

// readBlockHeader reads the track number, relative timecode and flags of a
// block whose body ends at end.
func (r *reader) readBlockHeader(end int64) (blockHeader, error) {
	track, err := r.readVint()
	if err != nil {
		return blockHeader{}, err
	}
	buf, err := r.readFull(3)
	if err != nil {
		return blockHeader{}, err
	}
	h := blockHeader{
		track:    track,
		timecode: int16(binary.BigEndian.Uint16(buf[:2])),
		flags:    buf[2],
		bodyLen:  end - r.pos,
	}
	if h.bodyLen < 0 {
		return blockHeader{}, fmt.Errorf("block at byte %d is shorter than its header", end)
	}
	return h, nil
}
