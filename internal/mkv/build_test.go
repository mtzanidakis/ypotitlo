package mkv

import (
	"encoding/binary"
	"testing"
)

// A minimal EBML writer, for tests only.
//
// Fixtures are built rather than committed because the cases worth pinning —
// a laced block, a cluster with no timestamp, a segment of unknown size, a
// NUL-padded language — are cases no muxer will produce on request. The one
// thing a builder cannot prove is that this package agrees with a real muxer,
// which is what testdata/sample.mkv is for.

// encodeID writes an element ID in its canonical form, marker included.
func encodeID(id uint32) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// encodeVint writes a value in the VINT encoding EBML uses for element sizes
// and for a block's track number, in the shortest form that is not the
// reserved all-ones pattern.
func encodeVint(v uint64) []byte {
	for n := 1; n <= 8; n++ {
		limit := uint64(1)<<(7*n) - 1
		if v >= limit {
			continue
		}
		out := make([]byte, n)
		x := v | 1<<(7*n)
		for i := n - 1; i >= 0; i-- {
			out[i] = byte(x)
			x >>= 8
		}
		return out
	}
	panic("value too large for a vint")
}

// unknownVint is the reserved all-ones size, meaning "ends with the parent".
func unknownVint() []byte { return []byte{0xFF} }

// elem builds one element from its body.
func elem(id uint32, body []byte) []byte {
	out := append([]byte{}, encodeID(id)...)
	out = append(out, encodeVint(uint64(len(body)))...)
	return append(out, body...)
}

// elemUnknownSize builds an element that declares an unknown size.
func elemUnknownSize(id uint32, body []byte) []byte {
	out := append([]byte{}, encodeID(id)...)
	out = append(out, unknownVint()...)
	return append(out, body...)
}

// uintElem builds an unsigned integer element, big-endian and minimal-width.
func uintElem(id uint32, v uint64) []byte {
	var body []byte
	if v == 0 {
		body = []byte{0}
	}
	for shift := 56; shift >= 0 && v != 0; shift -= 8 {
		b := byte(v >> shift)
		if len(body) == 0 && b == 0 {
			continue
		}
		body = append(body, b)
	}
	return elem(id, body)
}

// strElem builds a string element.
func strElem(id uint32, s string) []byte { return elem(id, []byte(s)) }

// cat concatenates element byte slices.
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ebmlHeader builds an EBML header declaring docType.
func ebmlHeader(docType string) []byte {
	if docType == "" {
		return elem(idEBML, nil)
	}
	return elem(idEBML, strElem(idDocType, docType))
}

// blockBody builds the payload of a Block or SimpleBlock: the track number,
// the timecode relative to the cluster, the flags byte and the text.
func blockBody(track uint64, timecode int16, flags byte, text string) []byte {
	out := append([]byte{}, encodeVint(track)...)
	var ts [2]byte
	binary.BigEndian.PutUint16(ts[:], uint16(timecode))
	out = append(out, ts[0], ts[1], flags)
	return append(out, text...)
}

// blockGroup builds a BlockGroup carrying one Block and its duration. A
// negative duration omits BlockDuration, which is what leaves a cue with no
// end time in the file.
func blockGroup(track uint64, timecode int16, duration int64, text string) []byte {
	body := elem(idBlock, blockBody(track, timecode, 0, text))
	if duration >= 0 {
		body = append(body, uintElem(idBlockDuration, uint64(duration))...)
	}
	return elem(idBlockGroup, body)
}

// simpleBlock builds a SimpleBlock, which has nowhere to put a duration.
func simpleBlock(track uint64, timecode int16, text string) []byte {
	return elem(idSimpleBlock, blockBody(track, timecode, 0x80, text))
}

// cluster builds a Cluster with a timestamp and the given blocks.
func cluster(timestamp uint64, blocks ...[]byte) []byte {
	return elem(idCluster, cat(append([][]byte{uintElem(idClusterTime, timestamp)}, blocks...)...))
}

// trackOpt customises a TrackEntry.
type trackOpt func(*[]byte)

func withLanguage(s string) trackOpt {
	return func(b *[]byte) { *b = append(*b, strElem(idLanguage, s)...) }
}

func withLanguageBCP47(s string) trackOpt {
	return func(b *[]byte) { *b = append(*b, strElem(idLanguageBCP47, s)...) }
}

func withName(s string) trackOpt {
	return func(b *[]byte) { *b = append(*b, strElem(idTrackName, s)...) }
}

func withFlag(id uint32, on bool) trackOpt {
	return func(b *[]byte) {
		v := uint64(0)
		if on {
			v = 1
		}
		*b = append(*b, uintElem(id, v)...)
	}
}

// trackEntry builds a TrackEntry.
func trackEntry(number uint64, typ TrackType, codec string, opts ...trackOpt) []byte {
	body := cat(
		uintElem(idTrackNumber, number),
		uintElem(idTrackUID, number*1000),
		uintElem(idTrackType, uint64(typ)),
		strElem(idCodecID, codec),
	)
	for _, o := range opts {
		o(&body)
	}
	return elem(idTrackEntry, body)
}

// file assembles a whole Matroska file: an EBML header and a Segment holding
// Info, Tracks and the clusters.
func file(t *testing.T, scale uint64, tracks [][]byte, clusters ...[]byte) []byte {
	t.Helper()
	info := elem(idInfo, uintElem(idTimestampScale, scale))
	body := cat(append([][]byte{info, elem(idTracks, cat(tracks...))}, clusters...)...)
	return cat(ebmlHeader("matroska"), elem(idSegment, body))
}
