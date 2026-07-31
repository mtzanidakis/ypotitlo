package mkv

import (
	"os"
	"testing"
	"time"
)

// testdata/sample.mkv is a real file from a real muxer: ffmpeg, muxing a
// three-cue SubRip file alongside a tiny video track.
//
// The hand-built fixtures elsewhere in this package prove the parser against
// cases chosen to break it; this one proves it against what a muxer actually
// writes, which is the only claim the others cannot make. Blocks of the video
// track sit between the subtitle blocks, so it is also the test that the
// track filter is doing something.
func TestSampleFileFromFFmpeg(t *testing.T) {
	t.Parallel()

	f, err := os.Open("testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	m, err := NewReader(f)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := m.DocType(); got != "matroska" {
		t.Errorf("doctype = %q, want matroska", got)
	}
	if got := m.TimestampScale(); got != time.Millisecond {
		t.Errorf("timestamp scale = %v, want 1ms", got)
	}

	subs := m.SubtitleTracks()
	if len(subs) != 1 {
		t.Fatalf("subtitle tracks = %d, want 1: %+v", len(subs), m.Tracks())
	}
	// ffprobe calls this stream 1. Matroska calls it track 2. A tool that
	// prints one and accepts the other extracts the wrong track.
	if subs[0].Number != 2 {
		t.Errorf("track number = %d, want 2", subs[0].Number)
	}
	if subs[0].Language != "eng" || subs[0].LanguageIsDefault {
		t.Errorf("language = %q (assumed %v), want a real %q", subs[0].Language, subs[0].LanguageIsDefault, "eng")
	}
	if subs[0].CodecID != CodecUTF8 {
		t.Errorf("codec = %q, want %q", subs[0].CodecID, CodecUTF8)
	}

	res, err := m.Cues(subs[0].Number)
	if err != nil {
		t.Fatalf("Cues: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", res.Warnings)
	}

	want := []Cue{
		{Start: 1500 * time.Millisecond, End: 3250 * time.Millisecond, Text: "First cue."},
		{Start: 4 * time.Second, End: 6125 * time.Millisecond, Text: "Second cue,\ntwo lines."},
		{Start: 70001 * time.Millisecond, End: 72999 * time.Millisecond, Text: "<i>Third</i> & last."},
	}
	if len(res.Cues) != len(want) {
		t.Fatalf("cues = %d, want %d: %+v", len(res.Cues), len(want), res.Cues)
	}
	for i, w := range want {
		got := res.Cues[i]
		if got.Start != w.Start || got.End != w.End {
			t.Errorf("cue %d timing = %v --> %v, want %v --> %v", i, got.Start, got.End, w.Start, w.End)
		}
		// The markup and the bare ampersand are the point: nothing here
		// interprets cue text, so they arrive as they were written.
		if got.Text != w.Text {
			t.Errorf("cue %d text = %q, want %q", i, got.Text, w.Text)
		}
		if got.InferredEnd {
			t.Errorf("cue %d: ffmpeg writes BlockDuration, so no end should be inferred", i)
		}
	}
}

// testdata/tracks.mkv is four subtitle tracks muxed by ffmpeg, with the
// dispositions a real file carries.
//
// It exists because the hand-built fixtures cannot prove agreement about
// element IDs or specified defaults: a wrong ID would be written and read back
// consistently by the same builder, and a test would pass on it. Only a file
// somebody else wrote can settle what 0x88 and 0x55AA mean, and what an absent
// element means.
func TestTrackFlagsFromFFmpeg(t *testing.T) {
	t.Parallel()

	f, err := os.Open("testdata/tracks.mkv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	m, err := NewReader(f)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	subs := m.SubtitleTracks()
	if len(subs) != 4 {
		t.Fatalf("subtitle tracks = %d, want 4: %+v", len(subs), subs)
	}

	want := []struct {
		number   uint64
		language string
		name     string
		dflt     bool
		forced   bool
	}{
		// FlagDefault's specified default is 1, so ffmpeg writes the
		// element only on the tracks that are *not* default. Reading an
		// absent FlagDefault as false makes every track in the file
		// non-default, silently: nothing is missing and nothing fails.
		{2, "eng", "English", true, false},
		{3, "eng", "Signs", false, true},
		{4, "eng", "English SDH", false, false},
		{5, "ell", "Greek", false, false},
	}
	for i, w := range want {
		got := subs[i]
		if got.Number != w.number || got.Language != w.language || got.Name != w.name {
			t.Errorf("track %d = %d/%q/%q, want %d/%q/%q",
				i, got.Number, got.Language, got.Name, w.number, w.language, w.name)
		}
		if got.Default != w.dflt {
			t.Errorf("track %d default = %v, want %v", got.Number, got.Default, w.dflt)
		}
		if got.Forced != w.forced {
			t.Errorf("track %d forced = %v, want %v", got.Number, got.Forced, w.forced)
		}
		if got.LanguageIsDefault {
			t.Errorf("track %d: the file tags a language, so nothing was assumed", got.Number)
		}
	}
}
