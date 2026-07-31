package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/mkv"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// sampleMKV copies the fixture into a temporary directory under the given
// name, so that a test which writes a derived output file writes it there and
// not into testdata.
func sampleMKV(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole point of the command: a container in, a subtitle out, with the
// timings, the markup and the bare ampersand exactly as the container held
// them.
func TestExtractWritesADerivedFile(t *testing.T) {
	t.Parallel()

	in := sampleMKV(t, "movie.mkv")
	e, _, errb := testEnv(t, []string{"extract", "-i", in}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got, exitOK, errb)
	}

	out := filepath.Join(filepath.Dir(in), "movie.en.srt")
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("derived output %s: %v", out, err)
	}

	f, err := srt.ParseBytes(raw)
	if err != nil {
		t.Fatalf("the extracted file does not parse: %v", err)
	}
	if len(f.Cues) != 3 {
		t.Fatalf("cues = %d, want 3", len(f.Cues))
	}
	if len(f.Warnings) != 0 {
		t.Errorf("re-parsing the output warned: %q", f.Warnings)
	}
	if got, want := f.Cues[0].Start, 1500*time.Millisecond; got != want {
		t.Errorf("first start = %v, want %v", got, want)
	}
	if got, want := f.Cues[1].Lines, []string{"Second cue,", "two lines."}; !slices.Equal(got, want) {
		t.Errorf("second cue lines = %q, want %q", got, want)
	}
	if got, want := f.Cues[2].Lines, []string{"<i>Third</i> & last."}; !slices.Equal(got, want) {
		t.Errorf("third cue lines = %q, want %q; markup and & must survive", got, want)
	}
	if !strings.Contains(errb.String(), "3 cues") {
		t.Errorf("stderr = %q, want it to report the cue count", errb)
	}
}

// An extracted file is the input to `translate`, so the two commands have to
// agree on names: movie.en.srt must derive movie.el.srt, not movie.en.el.srt.
func TestExtractedNameFeedsTranslate(t *testing.T) {
	t.Parallel()

	greek, err := lang.Resolve("el")
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := lang.SidecarPath("movie.mkv", mustLang(t, "eng"))
	if err != nil {
		t.Fatal(err)
	}
	if sidecar != "movie.en.srt" {
		t.Fatalf("sidecar = %q, want movie.en.srt", sidecar)
	}
	got, err := lang.DeriveOutputPath(sidecar, greek)
	if err != nil {
		t.Fatal(err)
	}
	if want := "movie.el.srt"; got != want {
		t.Errorf("translate would write %q, want %q", got, want)
	}
}

func mustLang(t *testing.T, s string) lang.Lang {
	t.Helper()
	l, err := lang.Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestExtractListsTracks(t *testing.T) {
	t.Parallel()

	in := sampleMKV(t, "movie.mkv")
	e, out, errb := testEnv(t, []string{"extract", "-i", in, "-list"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got, exitOK, errb)
	}

	s := out.String()
	for _, want := range []string{"TRACK", "LANGUAGE", "SubRip text", "\n2\t", "-il LANGUAGE"} {
		if !strings.Contains(strings.ReplaceAll(s, "  ", "\t"), strings.ReplaceAll(want, "  ", "\t")) {
			t.Errorf("listing = %q, want it to contain %q", s, want)
		}
	}
	// Listing must not write anything.
	if _, err := os.Stat(filepath.Join(filepath.Dir(in), "movie.en.srt")); err == nil {
		t.Error("-list wrote a file")
	}
}

func TestExtractRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    func(dir string) []string
		want    int
		wantErr string
	}{
		{
			name:    "missing input",
			args:    func(string) []string { return []string{"extract"} },
			want:    exitUsage,
			wantErr: "-i is required",
		},
		{
			name:    "stdin",
			args:    func(string) []string { return []string{"extract", "-i", "-"} },
			want:    exitUsage,
			wantErr: "cannot read stdin",
		},
		{
			name: "not a container",
			args: func(dir string) []string {
				p := filepath.Join(dir, "movie.srt")
				if err := os.WriteFile(p, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi.\n"), 0o644); err != nil {
					panic(err)
				}
				return []string{"extract", "-i", p}
			},
			want:    exitUsage,
			wantErr: "Matroska containers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, _, errb := testEnv(t, tt.args(t.TempDir()), nil)
			if got := run(context.Background(), e); got != tt.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got, tt.want, errb)
			}
			if !strings.Contains(errb.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errb, tt.wantErr)
			}
		})
	}
}

// The derived name is the input's own name plus a language, so it cannot be the
// input; but -o can be pointed anywhere, and an existing file is not ours to
// replace without being told.
func TestExtractRefusesToClobber(t *testing.T) {
	t.Parallel()

	in := sampleMKV(t, "movie.mkv")
	out := filepath.Join(filepath.Dir(in), "movie.en.srt")
	if err := os.WriteFile(out, []byte("PRECIOUS"), 0o644); err != nil {
		t.Fatal(err)
	}

	e, _, errb := testEnv(t, []string{"extract", "-i", in}, nil)
	if got := run(context.Background(), e); got != exitUsage {
		t.Fatalf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(errb.String(), "already exists") {
		t.Errorf("stderr = %q, want it to name the existing file", errb)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "PRECIOUS" {
		t.Error("the existing file was overwritten anyway")
	}

	e, _, errb = testEnv(t, []string{"extract", "-i", in, "-f"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("-f should permit the overwrite: exit = %d (stderr: %s)", got, errb)
	}
}

// text builds a readable subtitle track for the selection tests.
func text(n uint64, language string, opts ...func(*mkv.Track)) mkv.Track {
	t := mkv.Track{Number: n, Type: mkv.TrackSubtitle, CodecID: mkv.CodecUTF8, Language: language}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func forced(t *mkv.Track)    { t.Forced = true }
func sdh(t *mkv.Track)       { t.HearingImpaired = true }
func isDefault(t *mkv.Track) { t.Default = true }

func named(name string) func(*mkv.Track) {
	return func(t *mkv.Track) { t.Name = name }
}

// pick runs the selection the way the command does: resolve -il, then fall
// back to the tag as written when it does not resolve.
func pick(subs []mkv.Track, il string) (mkv.Track, []string, error) {
	want, _ := lang.Resolve(il)
	return pickTrack(subs, want, strings.TrimSpace(il), false, false)
}

// pickMarked is pick with -forced and -sdh.
func pickMarked(subs []mkv.Track, il string, forced, sdh bool) (mkv.Track, []string, error) {
	want, _ := lang.Resolve(il)
	return pickTrack(subs, want, strings.TrimSpace(il), forced, sdh)
}

func TestPickTrack(t *testing.T) {
	t.Parallel()

	ass := mkv.Track{Number: 4, Type: mkv.TrackSubtitle, CodecID: mkv.CodecASS, Language: "eng"}
	pgs := mkv.Track{Number: 5, Type: mkv.TrackSubtitle, CodecID: mkv.CodecPGS, Language: "ell"}

	tests := []struct {
		name     string
		subs     []mkv.Track
		il       string
		want     uint64
		wantNote string
		wantErr  string
	}{
		{name: "the only text track needs no -il", subs: []mkv.Track{text(2, "eng")}, want: 2},
		{name: "the only text track among bitmaps", subs: []mkv.Track{pgs, text(2, "eng")}, want: 2},
		{name: "no subtitle tracks at all", subs: nil, wantErr: "no subtitle tracks"},

		// A language is what the user has an opinion about, and every
		// spelling of it is the same request.
		{name: "by code", subs: []mkv.Track{text(2, "eng"), text(3, "ell")}, il: "el", want: 3},
		{name: "by 639-2/B code", subs: []mkv.Track{text(2, "eng"), text(3, "ell")}, il: "gre", want: 3},
		{name: "by name", subs: []mkv.Track{text(2, "eng"), text(3, "ell")}, il: "greek", want: 3},
		{name: "by native name", subs: []mkv.Track{text(2, "eng"), text(3, "ell")}, il: "Ελληνικά", want: 3},
		{name: "case-insensitive", subs: []mkv.Track{text(2, "eng"), text(3, "ell")}, il: "GREEK", want: 3},
		// A tag this tool cannot resolve is still a tag the file has, and
		// still the track the user is pointing at.
		{name: "by an unresolvable tag", subs: []mkv.Track{text(2, "eng"), text(3, "zxx")}, il: "zxx", want: 3},

		// Which language each track is in is the one thing the command
		// cannot work out on its own.
		{
			name:    "several languages and no -il",
			subs:    []mkv.Track{text(2, "eng"), text(3, "ell")},
			wantErr: "-il el",
		},
		{
			name:    "a language the file does not have",
			subs:    []mkv.Track{text(2, "eng"), text(3, "ell")},
			il:      "french",
			wantErr: `no subtitle track for "french"`,
		},
		{
			name:    "a mistyped language names what is there",
			subs:    []mkv.Track{text(2, "eng")},
			il:      "enlgish",
			wantErr: "English (-il en)",
		},

		// Given the language, the rest is decided rather than asked. A
		// forced track is signs only and is never the one meant for
		// translation, even when the file marks it default.
		{
			name:     "forced loses to the full track",
			subs:     []mkv.Track{text(2, "eng", forced, isDefault), text(3, "eng")},
			il:       "en",
			want:     3,
			wantNote: "leaving track 2",
		},
		{
			name:     "sdh loses to the full track",
			subs:     []mkv.Track{text(2, "eng", sdh), text(3, "eng")},
			il:       "en",
			want:     3,
			wantNote: "2 tracks match",
		},
		{
			name:     "the file's default breaks the tie",
			subs:     []mkv.Track{text(2, "eng"), text(3, "eng", isDefault)},
			il:       "en",
			want:     3,
			wantNote: "extracting track 3",
		},
		{
			name:     "the track number settles the rest",
			subs:     []mkv.Track{text(9, "eng"), text(3, "eng")},
			il:       "en",
			want:     3,
			wantNote: "2 tracks match",
		},
		// One language throughout is not ambiguous, so -il is not needed
		// to get past a forced companion track.
		{
			name:     "one language, several tracks, no -il",
			subs:     []mkv.Track{text(2, "eng", forced), text(3, "eng")},
			want:     3,
			wantNote: "2 tracks match",
		},

		// Asking for a language carried only by a track we cannot read
		// has to explain that, not claim the language is absent.
		{name: "ass names mkvextract", subs: []mkv.Track{ass}, il: "en", wantErr: "mkvextract"},
		{name: "bitmaps name OCR", subs: []mkv.Track{pgs}, il: "el", wantErr: "OCR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, notes, err := pick(tt.subs, tt.il)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pickTrack: %v", err)
			}
			if got.Number != tt.want {
				t.Errorf("track = %d, want %d", got.Number, tt.want)
			}
			if tt.wantNote == "" {
				if len(notes) != 0 {
					t.Errorf("notes = %q, want none", notes)
				}
				return
			}
			if len(notes) == 0 || !strings.Contains(strings.Join(notes, "\n"), tt.wantNote) {
				t.Errorf("notes = %q, want one containing %q", notes, tt.wantNote)
			}
		})
	}
}

// Selecting between tracks is allowed, but a selection nobody is told about is
// exactly the silence this tool exists to avoid: the result is a plausible file
// that may be the wrong subtitle.
func TestAChoiceBetweenTracksIsAlwaysReported(t *testing.T) {
	t.Parallel()

	subs := []mkv.Track{text(2, "eng", forced, isDefault), text(3, "eng"), text(4, "eng", sdh)}
	got, notes, err := pick(subs, "en")
	if err != nil {
		t.Fatalf("pickTrack: %v", err)
	}
	if got.Number != 3 {
		t.Fatalf("track = %d, want the full track 3", got.Number)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"3 tracks match", "extracting track 3", "track 2", "forced", "track 4", "sdh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %q, want them to mention %q", joined, want)
		}
	}
}

// -forced and -sdh are what make the tracks the automatic choice deliberately
// skips reachable at all.
func TestForcedAndSDHSelectTheOtherTracks(t *testing.T) {
	t.Parallel()

	full := text(2, "eng", isDefault)
	forcedTrack := text(3, "eng", forced, named("Signs"))
	sdhTrack := text(4, "eng", sdh)
	greek := text(5, "ell")
	subs := []mkv.Track{full, forcedTrack, sdhTrack, greek}

	tests := []struct {
		name   string
		il     string
		forced bool
		sdh    bool
		want   uint64
	}{
		{name: "neither takes the full track", il: "en", want: 2},
		{name: "forced", il: "en", forced: true, want: 3},
		{name: "sdh", il: "en", sdh: true, want: 4},
		// The language can be left out when the markers alone are enough
		// to narrow it to one track.
		{name: "forced without -il", forced: true, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := pickMarked(subs, tt.il, tt.forced, tt.sdh)
			if err != nil {
				t.Fatalf("pickTrack: %v", err)
			}
			if got.Number != tt.want {
				t.Errorf("track = %d, want %d", got.Number, tt.want)
			}
		})
	}
}

// A filter, not a preference. Asking for a track the file does not have is a
// mistake worth reporting; quietly handing back the nearest other track would
// produce a plausible file that is the wrong subtitle.
func TestForcedAndSDHRefuseToSubstitute(t *testing.T) {
	t.Parallel()

	subs := []mkv.Track{text(2, "eng", isDefault), text(3, "ell")}

	_, _, err := pickMarked(subs, "en", false, true)
	if err == nil || !strings.Contains(err.Error(), "no hearing-impaired") {
		t.Fatalf("error = %v, want it to say there is no such track", err)
	}
	// The refusal has to say what the file does have, flags included.
	if !strings.Contains(err.Error(), "track 2") {
		t.Errorf("error = %v, want it to list the tracks", err)
	}

	_, _, err = pickMarked(subs, "el", true, false)
	if err == nil || !strings.Contains(err.Error(), `no forced subtitle track for "el"`) {
		t.Fatalf("error = %v, want it to repeat the request back", err)
	}
}

// FlagHearingImpaired was added to Matroska long after the files people own
// were muxed, so a great many rips record the fact only in the track's title.
// Reading the title is what makes -sdh find the track — and what keeps the
// output from colliding with the ordinary one.
func TestMarkersAreReadFromTheTrackName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		track      mkv.Track
		wantForced bool
		wantSDH    bool
	}{
		{name: "plain", track: text(2, "eng", named("English"))},
		{name: "SDH in the name", track: text(2, "eng", named("English SDH")), wantSDH: true},
		{name: "spelled out", track: text(2, "eng", named("English (Hearing Impaired)")), wantSDH: true},
		{name: "CC", track: text(2, "eng", named("English CC")), wantSDH: true},
		{name: "forced", track: text(2, "eng", named("English Forced")), wantForced: true},
		{name: "signs", track: text(2, "eng", named("Signs & Songs")), wantForced: true},
		// Whole words only. Substring matching finds "hi" in "This" and
		// "cc" in "Occitan", and would mark half a library.
		{name: "hi inside a word", track: text(2, "eng", named("This is the one"))},
		{name: "cc inside a word", track: text(2, "oci", named("Occitan"))},
		{name: "no name at all", track: text(2, "eng")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := trackIsForced(tt.track); got != tt.wantForced {
				t.Errorf("trackIsForced(%q) = %v, want %v", tt.track.Name, got, tt.wantForced)
			}
			if got := trackIsSDH(tt.track); got != tt.wantSDH {
				t.Errorf("trackIsSDH(%q) = %v, want %v", tt.track.Name, got, tt.wantSDH)
			}
		})
	}
}

// A marker inferred from a name decides the output filename, so it cannot be
// inferred silently.
func TestAnInferredMarkerIsReportedAndNamesTheFile(t *testing.T) {
	t.Parallel()

	subs := []mkv.Track{text(2, "eng"), text(3, "eng", named("English SDH"))}
	got, notes, err := pickMarked(subs, "en", false, true)
	if err != nil {
		t.Fatalf("pickTrack: %v", err)
	}
	if got.Number != 3 {
		t.Fatalf("track = %d, want 3", got.Number)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "no hearing-impaired flag") || !strings.Contains(joined, "English SDH") {
		t.Errorf("notes = %q, want one saying the marker came from the name", notes)
	}

	path, err := resolveExtractPath(extractFlags{in: "movie.mkv"}, got, mustLang(t, "eng"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "movie.en.sdh.srt"; path != want {
		t.Errorf("path = %q, want %q; an unflagged SDH track would otherwise overwrite the plain one", path, want)
	}
}

// The listing has to predict the filename an extraction will produce, and say
// which of the two sources each marker came from.
func TestListingMarksInferredFlags(t *testing.T) {
	t.Parallel()

	if got, want := trackFlags(text(2, "eng", sdh, isDefault)), "default,sdh"; got != want {
		t.Errorf("flags = %q, want %q", got, want)
	}
	if got, want := trackFlags(text(2, "eng", named("English SDH"))), "sdh?"; got != want {
		t.Errorf("flags = %q, want %q", got, want)
	}
	if got, want := trackFlags(text(2, "eng", named("Signs"))), "forced?"; got != want {
		t.Errorf("flags = %q, want %q", got, want)
	}
}

// A streaming rip carries forty subtitle tracks. An error that spells out all
// forty is one the reader scrolls past rather than reads, so past a cap it
// gives the count and points at the command that exists to show them.
func TestManyLanguagesDeferToTheListing(t *testing.T) {
	t.Parallel()

	codes := []string{"eng", "ara", "bul", "ces", "dan", "deu", "ell", "spa", "est", "fin", "fra", "heb"}
	var many []mkv.Track
	for i, c := range codes {
		many = append(many, text(uint64(i+2), c))
	}
	few := many[:3]

	tests := []struct {
		name    string
		subs    []mkv.Track
		il      string
		wantErr string
		notWant string
	}{
		{
			name:    "many languages, no -il",
			subs:    many,
			wantErr: fmt.Sprintf("subtitles in %d languages; run -list", len(codes)),
			notWant: "Bulgarian",
		},
		{
			name:    "many languages, a language the file lacks",
			subs:    many,
			il:      "japanese",
			wantErr: "-list shows them",
			notWant: "Bulgarian",
		},
		// Below the cap the whole list is the useful part: it can be
		// pasted straight into -il.
		{
			name:    "few languages are spelled out",
			subs:    few,
			wantErr: "Bulgarian (-il bg)",
		},
		{
			name:    "few languages, a language the file lacks",
			subs:    few,
			il:      "japanese",
			wantErr: "Arabic (-il ar)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := pick(tt.subs, tt.il)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error = %v, want it not to spell out %q", err, tt.notWant)
			}
		})
	}
}

// A tag this tool cannot resolve still selects a track, so an error that named
// the tag without saying how to ask for it would be a dead end. Real files are
// full of them: a rip with three "und" tracks is three Chinese subtitles the
// muxer failed to tag.
func TestUnresolvableTagsAreStillOffered(t *testing.T) {
	t.Parallel()

	subs := []mkv.Track{text(2, "eng"), text(3, "und"), text(4, "und")}
	_, _, err := pick(subs, "japanese")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "-il und") {
		t.Errorf("error = %v, want it to offer -il und", err)
	}

	// And it has to actually work.
	got, notes, err := pick(subs, "und")
	if err != nil {
		t.Fatalf("pickTrack: %v", err)
	}
	if got.Number != 3 {
		t.Errorf("track = %d, want 3", got.Number)
	}
	if len(notes) == 0 {
		t.Error("choosing between two und tracks must be reported")
	}
}

// The same file has to yield the same subtitle every run. A map iteration or an
// unstable sort in the selection would make it a coin toss that only shows up
// as an occasional wrong file.
func TestSelectionIsDeterministic(t *testing.T) {
	t.Parallel()

	subs := []mkv.Track{text(7, "eng"), text(3, "eng"), text(5, "eng"), text(9, "eng")}
	for range 50 {
		got, _, err := pick(subs, "en")
		if err != nil {
			t.Fatal(err)
		}
		if got.Number != 3 {
			t.Fatalf("track = %d, want 3 every time", got.Number)
		}
	}
}

// The forced and the full track of one language derive the same filename
// unless the flags become markers, and one would overwrite the other.
func TestExtractPathKeepsTrackMarkers(t *testing.T) {
	t.Parallel()

	english := mustLang(t, "eng")
	tests := []struct {
		name  string
		track mkv.Track
		want  string
	}{
		{name: "plain", track: mkv.Track{}, want: "movie.en.srt"},
		{name: "forced", track: mkv.Track{Forced: true}, want: "movie.en.forced.srt"},
		{name: "sdh", track: mkv.Track{HearingImpaired: true}, want: "movie.en.sdh.srt"},
		{name: "both", track: mkv.Track{Forced: true, HearingImpaired: true}, want: "movie.en.forced.sdh.srt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveExtractPath(extractFlags{in: "movie.mkv"}, tt.track, english)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}

	// -o wins outright.
	got, err := resolveExtractPath(extractFlags{in: "movie.mkv", out: "elsewhere.srt"}, mkv.Track{Forced: true}, english)
	if err != nil || got != "elsewhere.srt" {
		t.Errorf("-o gave %q, %v", got, err)
	}
}

// A track tagged with something this tool does not translate is still worth
// extracting. It loses the language segment of the name and says why.
func TestUnknownTrackLanguageIsNotFatal(t *testing.T) {
	t.Parallel()

	tr := mkv.Track{Language: "zxx"}
	l, note := resolveTrackLang(tr)
	if !l.Zero() {
		t.Errorf("language = %v, want the zero Lang", l)
	}
	if !strings.Contains(note, "zxx") {
		t.Errorf("note = %q, want it to name the tag", note)
	}

	got, err := resolveExtractPath(extractFlags{in: "movie.mkv"}, tr, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := "movie.srt"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// Matroska's default of "eng" fills in for a track the file never tagged, and
// that default ends up in a filename. It has to be visibly a guess.
func TestAssumedLanguageSaysSo(t *testing.T) {
	t.Parallel()

	_, note := resolveTrackLang(mkv.Track{Language: "eng", LanguageIsDefault: true})
	if !strings.Contains(note, "assumed") {
		t.Errorf("note = %q, want it to say the language was assumed", note)
	}
}

func TestSplitCueLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "one line", in: "Hello.", want: []string{"Hello."}},
		{name: "LF", in: "a\nb", want: []string{"a", "b"}},
		{name: "CRLF", in: "a\r\nb", want: []string{"a", "b"}},
		{name: "CR", in: "a\rb", want: []string{"a", "b"}},
		{name: "trailing LF is a muxer artefact", in: "a\n", want: []string{"a"}},
		{name: "trailing CRLF is a muxer artefact", in: "a\r\n", want: []string{"a"}},
		{name: "empty", in: "", want: []string{}},
		// Nothing is trimmed: leading whitespace is positioning, and
		// {\an8} and markup are text like any other.
		{name: "leading space survives", in: "   a", want: []string{"   a"}},
		{name: "markup survives", in: "{\\an8}<i>a</i> & b", want: []string{"{\\an8}<i>a</i> & b"}},
		{name: "interior blank line survives", in: "a\n\nb", want: []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitCueLines(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("lines = %q, want %q", got, tt.want)
			}
			if got == nil {
				t.Error("Lines must be non-nil, matching what the parser produces")
			}
		})
	}
}

// Many muxers store cue text with CRLF. Writing LF between the cues while
// copying CRLF inside them makes a mixed-ending file — which is what ffmpeg's
// stream copy produces, and what every later tool then has to decide about.
func TestCueLineEndingFollowsTheContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		texts    []string
		want     srt.LineEnding
		wantWarn bool
	}{
		{name: "no multi-line cues", texts: []string{"a", "b"}, want: srt.LF},
		{name: "LF", texts: []string{"a\nb"}, want: srt.LF},
		{name: "CRLF", texts: []string{"a\r\nb", "c\r\nd"}, want: srt.CRLF},
		{name: "CR", texts: []string{"a\rb"}, want: srt.CR},
		{name: "mixed takes the majority and says so", texts: []string{"a\r\nb", "c\r\nd", "e\nf"}, want: srt.CRLF, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cues := make([]mkv.Cue, len(tt.texts))
			for i, s := range tt.texts {
				cues[i] = mkv.Cue{Text: s}
			}
			f, warnings := buildSRT(cues)
			if f.LineEnding != tt.want {
				t.Errorf("line ending = %v, want %v", f.LineEnding, tt.want)
			}
			if got := len(warnings) > 0; got != tt.wantWarn {
				t.Errorf("warnings = %q, want any = %v", warnings, tt.wantWarn)
			}
		})
	}
}

// The whole file has to come back out, in order, with nothing invented and
// nothing dropped.
func TestBuildSRTPreservesEveryCue(t *testing.T) {
	t.Parallel()

	in := []mkv.Cue{
		{Start: time.Second, End: 2 * time.Second, Text: "one"},
		{Start: 3 * time.Second, End: 4 * time.Second, Text: ""},
		{Start: 5 * time.Second, End: 6 * time.Second, Text: "three"},
	}
	f, _ := buildSRT(in)
	if len(f.Cues) != len(in) {
		t.Fatalf("cues = %d, want %d; an empty cue is still a cue", len(f.Cues), len(in))
	}
	for i, c := range in {
		if f.Cues[i].Start != c.Start || f.Cues[i].End != c.End {
			t.Errorf("cue %d timing = %v/%v, want %v/%v", i, f.Cues[i].Start, f.Cues[i].End, c.Start, c.End)
		}
	}

	// The writer numbers from 1, so an extracted file is always
	// conventionally indexed no matter how the container was built.
	body, _, err := srt.WriteBytes(f, srt.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "1\n") {
		t.Errorf("output starts %q, want it to start at index 1", string(body[:8]))
	}
}
