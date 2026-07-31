package mkv

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// ms is a shorthand for a timing expressed the way the fixtures express them.
func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// open builds a Reader over an in-memory fixture.
func open(t *testing.T, raw []byte) *Reader {
	t.Helper()
	m, err := NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return m
}

func TestReadsTrackMetadata(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000, [][]byte{
		trackEntry(1, TrackVideo, "V_MPEGH/ISO/HEVC"),
		trackEntry(2, TrackSubtitle, CodecUTF8, withLanguage("gre"), withName("Full"), withFlag(idFlagDefault, true)),
		trackEntry(3, TrackSubtitle, CodecUTF8, withLanguage("eng"), withFlag(idFlagForced, true), withFlag(idFlagHearingImpaired, true)),
		trackEntry(4, TrackSubtitle, CodecASS),
	})

	m := open(t, raw)
	if got := len(m.Tracks()); got != 4 {
		t.Fatalf("tracks = %d, want 4", got)
	}
	subs := m.SubtitleTracks()
	if got := len(subs); got != 3 {
		t.Fatalf("subtitle tracks = %d, want 3", got)
	}

	greek := subs[0]
	if greek.Number != 2 || greek.Language != "gre" || greek.Name != "Full" || !greek.Default {
		t.Errorf("track 2 = %+v", greek)
	}
	if greek.LanguageIsDefault {
		t.Error("an explicit language must not be reported as the Matroska default")
	}
	if !greek.Text() {
		t.Error("S_TEXT/UTF8 should be readable as text")
	}

	forced := subs[1]
	if !forced.Forced || !forced.HearingImpaired {
		t.Errorf("track 3 flags = %+v", forced)
	}

	if subs[2].Text() {
		t.Error("S_TEXT/ASS must not be reported as text this package can read")
	}
	if got, want := subs[2].CodecName(), "ASS"; got != want {
		t.Errorf("codec name = %q, want %q", got, want)
	}
}

// A track with no language element is "eng" by specification, and that default
// is wrong often enough that it has to be distinguishable from a real tag: it
// ends up in a filename.
func TestAbsentLanguageIsReportedAsAssumed(t *testing.T) {
	t.Parallel()

	m := open(t, file(t, 1_000_000, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)}))
	tr := m.Tracks()[0]
	if tr.Language != "eng" {
		t.Errorf("language = %q, want the specified default %q", tr.Language, "eng")
	}
	if !tr.LanguageIsDefault {
		t.Error("an absent language must be flagged as assumed")
	}
}

// FlagDefault's specified default is 1, not 0, so a muxer writes the element
// only on the tracks that are *not* default. Reading an absent FlagDefault as
// false makes every track in a file non-default, and nothing about that fails:
// no element is missing and no parse breaks, the selection tiebreak just stops
// working.
func TestAbsentFlagDefaultMeansDefault(t *testing.T) {
	t.Parallel()

	m := open(t, file(t, 1_000_000, [][]byte{
		trackEntry(1, TrackSubtitle, CodecUTF8),
		trackEntry(2, TrackSubtitle, CodecUTF8, withFlag(idFlagDefault, false)),
		trackEntry(3, TrackSubtitle, CodecUTF8, withFlag(idFlagDefault, true)),
	}))

	for i, want := range []bool{true, false, true} {
		if got := m.Tracks()[i].Default; got != want {
			t.Errorf("track %d default = %v, want %v", i+1, got, want)
		}
	}

	// The other two flags default to 0, so absent really is false there.
	tr := m.Tracks()[0]
	if tr.Forced || tr.HearingImpaired {
		t.Errorf("absent forced/hearing-impaired flags read as %v/%v, want false", tr.Forced, tr.HearingImpaired)
	}
}

// LanguageBCP47 exists because the old element cannot express pt-BR or
// zh-Hant, so where both are present the new one has to win.
func TestBCP47LanguageWinsOverTheLegacyElement(t *testing.T) {
	t.Parallel()

	m := open(t, file(t, 1_000_000, [][]byte{
		trackEntry(1, TrackSubtitle, CodecUTF8, withLanguage("por"), withLanguageBCP47("pt-BR")),
	}))
	if got := m.Tracks()[0].Language; got != "pt-BR" {
		t.Errorf("language = %q, want pt-BR", got)
	}
	if m.Tracks()[0].LanguageIsDefault {
		t.Error("a BCP-47 tag is not the Matroska default")
	}
}

// EBML permits an ASCII string to be padded with NUL bytes so it can be
// rewritten in place. A language of "eng\x00" resolves to nothing and would
// land in a filename.
func TestTrailingNULsAreStrippedFromStrings(t *testing.T) {
	t.Parallel()

	entry := elem(idTrackEntry, cat(
		uintElem(idTrackNumber, 1),
		uintElem(idTrackType, uint64(TrackSubtitle)),
		strElem(idCodecID, CodecUTF8+"\x00"),
		strElem(idLanguage, "eng\x00\x00"),
	))
	m := open(t, file(t, 1_000_000, [][]byte{entry}))
	tr := m.Tracks()[0]
	if tr.Language != "eng" {
		t.Errorf("language = %q, want %q", tr.Language, "eng")
	}
	if tr.CodecID != CodecUTF8 {
		t.Errorf("codec = %q, want %q", tr.CodecID, CodecUTF8)
	}
}

func TestReadsCues(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000,
		[][]byte{
			trackEntry(1, TrackVideo, "V_MPEG4/ISO/AVC"),
			trackEntry(2, TrackSubtitle, CodecUTF8, withLanguage("eng")),
		},
		cluster(1000,
			// A video block on the same track number space, to prove
			// blocks of other tracks are stepped over rather than read.
			simpleBlock(1, 0, "\x00\x01\x02not a subtitle"),
			blockGroup(2, 65, 2000, "Hello."),
			blockGroup(2, 3000, 1500, "Second line\nwrapped."),
		),
		cluster(10000,
			blockGroup(2, -500, 900, "Before its cluster."),
		),
	)

	m := open(t, raw)
	res, err := m.Cues(2)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", res.Warnings)
	}

	want := []Cue{
		{Start: ms(1065), End: ms(3065), Text: "Hello."},
		{Start: ms(4000), End: ms(5500), Text: "Second line\nwrapped."},
		{Start: ms(9500), End: ms(10400), Text: "Before its cluster."},
	}
	if len(res.Cues) != len(want) {
		t.Fatalf("cues = %d, want %d: %+v", len(res.Cues), len(want), res.Cues)
	}
	for i, w := range want {
		got := res.Cues[i]
		if got.Start != w.Start || got.End != w.End || got.Text != w.Text {
			t.Errorf("cue %d = %v/%v %q, want %v/%v %q", i, got.Start, got.End, got.Text, w.Start, w.End, w.Text)
		}
	}
	if res.Track.Number != 2 {
		t.Errorf("result track = %d, want 2", res.Track.Number)
	}
}

// TimestampScale is what a tick means, so reading it wrong scales every timing
// in the file by the same wrong factor — which looks plausible and is not.
func TestTimestampScaleIsApplied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scale uint64
		want  time.Duration
	}{
		{"milliseconds", 1_000_000, ms(1100)},
		{"hundred microseconds", 100_000, ms(110)},
		{"one second", 1_000_000_000, 1100 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := file(t, tc.scale,
				[][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)},
				cluster(1000, blockGroup(1, 100, 10, "x")),
			)
			res, err := open(t, raw).Cues(1)
			if err != nil {
				t.Fatalf("Cues: %v", err)
			}
			if got := res.Cues[0].Start; got != tc.want {
				t.Errorf("start = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnusableTimestampScaleIsRejected(t *testing.T) {
	t.Parallel()

	for _, scale := range []uint64{0, 2_000_000_000} {
		raw := file(t, scale, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)})
		if _, err := NewReader(bytes.NewReader(raw)); err == nil {
			t.Errorf("scale %d was accepted", scale)
		}
	}
}

// A SimpleBlock has nowhere to put a duration, so a cue muxed into one has no
// end time at all. Inventing one silently would be the worst answer; refusing
// the file would be nearly as bad.
func TestInferredEndsAreBoundedAndReported(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000,
		[][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)},
		cluster(0,
			simpleBlock(1, 1000, "next cue is close"),
			simpleBlock(1, 3000, "next cue is an hour away"),
		),
		// A block timecode is a signed 16-bit offset from its cluster, so
		// an hour later means a new cluster. That bound is the reason
		// clusters exist at all.
		cluster(3_600_000, simpleBlock(1, 3000, "last cue of the file")),
	)

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}

	want := []time.Duration{ms(3000), ms(3000) + maxInferredDuration, ms(3_603_000) + fallbackDuration}
	for i, w := range want {
		if got := res.Cues[i].End; got != w {
			t.Errorf("cue %d end = %v, want %v", i, got, w)
		}
		if !res.Cues[i].InferredEnd {
			t.Errorf("cue %d should be marked as having an inferred end", i)
		}
	}
	if !hasWarning(res.Warnings, "carried no duration") {
		t.Errorf("warnings = %q, want one naming the missing durations", res.Warnings)
	}
}

// The specification fixes no order between Block and BlockDuration inside a
// BlockGroup. A reader that assumed Block came first would drop every duration
// in a file written the other way, and report every cue as inferred.
func TestBlockDurationBeforeBlockIsStillFound(t *testing.T) {
	t.Parallel()

	group := elem(idBlockGroup, cat(
		uintElem(idBlockDuration, 2500),
		elem(idBlock, blockBody(1, 100, 0, "Reversed.")),
	))
	raw := file(t, 1_000_000, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)}, cluster(0, group))

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if got, want := res.Cues[0].End, ms(2600); got != want {
		t.Errorf("end = %v, want %v", got, want)
	}
	if res.Cues[0].InferredEnd {
		t.Error("the duration was present and must not be reported as inferred")
	}
}

// Cue count and order are part of the contract everywhere else in this tool,
// so a file whose blocks run backwards keeps them and says so.
func TestOutOfOrderCuesAreReportedNotSorted(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000,
		[][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)},
		cluster(0,
			blockGroup(1, 5000, 500, "second in time"),
			blockGroup(1, 1000, 500, "first in time"),
		),
	)

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if res.Cues[0].Text != "second in time" {
		t.Errorf("cues were reordered: %q", res.Cues[0].Text)
	}
	if !hasWarning(res.Warnings, "file order was kept") {
		t.Errorf("warnings = %q, want one naming the ordering", res.Warnings)
	}
}

// The declared codec says UTF-8. When the bytes disagree the bytes are kept —
// this tool does not repair text — but the disagreement is reported, because a
// user is about to send the file to a model and then publish it.
func TestInvalidUTF8IsReportedNotRepaired(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000,
		[][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)},
		cluster(0, blockGroup(1, 0, 500, "caf\xe9")),
	)

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if got, want := res.Cues[0].Text, "caf\xe9"; got != want {
		t.Errorf("text = %q, want the bytes unchanged %q", got, want)
	}
	if !hasWarning(res.Warnings, "not valid UTF-8") {
		t.Errorf("warnings = %q, want one naming the encoding", res.Warnings)
	}
}

// Lacing packs several frames into one block with no timestamp of their own.
// No muxer produces it for a subtitle, so an error naming it beats untested
// code guessing at a split.
func TestLacedBlockIsRefused(t *testing.T) {
	t.Parallel()

	laced := elem(idSimpleBlock, blockBody(1, 0, 0x80|0x02, "a\x00b"))
	raw := file(t, 1_000_000, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)}, cluster(0, laced))

	_, err := open(t, raw).Cues(1)
	if err == nil || !strings.Contains(err.Error(), "lacing") {
		t.Fatalf("error = %v, want one naming lacing", err)
	}
}

// An .mp4 or an .avi has to fail on the one fact the user needs — wrong kind
// of file — rather than deep inside a parse of nonsense.
func TestNonMatroskaFilesAreNamedAsSuch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  []byte
	}{
		{"mp4", []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41")},
		{"empty", nil},
		{"text", []byte("1\n00:00:01,000 --> 00:00:02,000\nHi.\n")},
		{"wrong doctype", cat(ebmlHeader("webm2"), elem(idSegment, nil))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewReader(bytes.NewReader(tc.raw))
			if !errors.Is(err, ErrNotMatroska) {
				t.Fatalf("error = %v, want ErrNotMatroska", err)
			}
		})
	}
}

// WebM is Matroska with a narrower codec list, and it carries WebVTT subtitle
// tracks. Refusing it on the DocType alone would be refusing a file this
// package can read perfectly well.
func TestWebMIsAccepted(t *testing.T) {
	t.Parallel()

	raw := cat(ebmlHeader("webm"), elem(idSegment, cat(
		elem(idInfo, uintElem(idTimestampScale, 1_000_000)),
		elem(idTracks, trackEntry(1, TrackSubtitle, CodecWebVTT)),
	)))
	m := open(t, raw)
	if got := m.DocType(); got != "webm" {
		t.Errorf("doctype = %q, want webm", got)
	}
}

// A file muxed as it was recorded cannot know its own length, so the Segment
// declares an unknown size and ends at the end of the file.
func TestSegmentOfUnknownSizeIsRead(t *testing.T) {
	t.Parallel()

	raw := cat(ebmlHeader("matroska"), elemUnknownSize(idSegment, cat(
		elem(idInfo, uintElem(idTimestampScale, 1_000_000)),
		elem(idTracks, trackEntry(1, TrackSubtitle, CodecUTF8)),
		cluster(0, blockGroup(1, 500, 1000, "Streamed.")),
	)))

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Cues) != 1 || res.Cues[0].Text != "Streamed." {
		t.Fatalf("cues = %+v", res.Cues)
	}
}

// Tracks legitimately sits after the clusters in a file muxed in one pass. A
// reader that stopped at the first cluster would report no tracks at all.
func TestTracksAfterTheClustersAreFound(t *testing.T) {
	t.Parallel()

	body := cat(
		elem(idInfo, uintElem(idTimestampScale, 1_000_000)),
		cluster(0, blockGroup(7, 250, 750, "Late tracks.")),
		elem(idTracks, trackEntry(7, TrackSubtitle, CodecUTF8, withLanguage("eng"))),
	)
	raw := cat(ebmlHeader("matroska"), elem(idSegment, body))

	res, err := open(t, raw).Cues(7)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Cues) != 1 || res.Cues[0].Text != "Late tracks." {
		t.Fatalf("cues = %+v", res.Cues)
	}
}

// An interrupted download still holds most of its subtitle. Discarding it to
// report a tidier error is the same trade the translator refuses to make with a
// partial translation.
func TestTruncatedFileKeepsWhatItHas(t *testing.T) {
	t.Parallel()

	raw := file(t, 1_000_000,
		[][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)},
		cluster(0, blockGroup(1, 0, 500, "kept one")),
		cluster(1000, blockGroup(1, 0, 500, "kept two")),
		cluster(2000, blockGroup(1, 0, 500, "this one is cut off")),
	)

	res, err := open(t, raw[:len(raw)-12]).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("cues = %d, want the 2 that survived: %+v", len(res.Cues), res.Cues)
	}
	if !hasWarning(res.Warnings, "readable") {
		t.Errorf("warnings = %q, want one saying the file was truncated", res.Warnings)
	}
}

func TestUnknownTrackIsRefused(t *testing.T) {
	t.Parallel()

	m := open(t, file(t, 1_000_000, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)}))
	if _, err := m.Cues(9); !errors.Is(err, ErrNoSuchTrack) {
		t.Fatalf("error = %v, want ErrNoSuchTrack", err)
	}
}

// A file with tracks but no clusters is legal and holds no subtitles. It is not
// a parse failure, and saying so is more use than an empty result.
func TestFileWithNoClustersSaysSo(t *testing.T) {
	t.Parallel()

	res, err := open(t, file(t, 1_000_000, [][]byte{trackEntry(1, TrackSubtitle, CodecUTF8)})).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Cues) != 0 {
		t.Fatalf("cues = %+v, want none", res.Cues)
	}
	if !hasWarning(res.Warnings, "no clusters") {
		t.Errorf("warnings = %q, want one naming the absent clusters", res.Warnings)
	}
}

// Elements this package does not care about must cost a seek and nothing else.
// An attachment is where a font or a cover image lives, so it is routinely the
// largest thing in the file after the video.
func TestUnknownElementsAreSteppedOver(t *testing.T) {
	t.Parallel()

	const idAttachments = 0x1941A469
	junk := elem(idAttachments, bytes.Repeat([]byte{0xAB}, 4096))
	body := cat(
		elem(idVoid, make([]byte, 128)),
		elem(idInfo, uintElem(idTimestampScale, 1_000_000)),
		junk,
		elem(idTracks, trackEntry(1, TrackSubtitle, CodecUTF8)),
		cluster(0, blockGroup(1, 0, 500, "Found it.")),
	)
	raw := cat(ebmlHeader("matroska"), elem(idSegment, body))

	res, err := open(t, raw).Cues(1)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Cues) != 1 || res.Cues[0].Text != "Found it." {
		t.Fatalf("cues = %+v", res.Cues)
	}
}

// A child claiming more bytes than its parent has is corruption, and following
// it would read one element's body as another's.
func TestChildLongerThanItsParentIsRefused(t *testing.T) {
	t.Parallel()

	// A Tracks element declaring 8 bytes but holding a TrackEntry that
	// claims 200.
	inner := cat(encodeID(idTrackEntry), encodeVint(200))
	tracks := cat(encodeID(idTracks), encodeVint(uint64(len(inner))+4), inner, []byte{0, 0, 0, 0})
	raw := cat(ebmlHeader("matroska"), elem(idSegment, tracks))

	if _, err := NewReader(bytes.NewReader(raw)); err == nil {
		t.Fatal("an over-long child was accepted")
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
