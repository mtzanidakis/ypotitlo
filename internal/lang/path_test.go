package lang

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveOutputPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		target string
		want   string
	}{
		// The ordinary case: replace the language segment.
		{"two letter code", "movie.en.srt", "el", "movie.el.srt"},
		{"three letter code", "movie.eng.srt", "el", "movie.el.srt"},
		{"bibliographic code", "movie.ger.srt", "el", "movie.el.srt"},
		{"uppercase code", "movie.EN.srt", "el", "movie.el.srt"},
		{"target given as name", "movie.en.srt", "greek", "movie.el.srt"},
		{"target given as 639-2", "movie.en.srt", "gre", "movie.el.srt"},

		// P0 #1: markers are peeled before any language test and put back
		// afterwards. Without this, movie.sdh.srt becomes movie.el.srt and
		// overwrites the translation of movie.srt, because "sdh" parses as
		// Southern Kurdish.
		{"sdh only", "movie.sdh.srt", "el", "movie.el.sdh.srt"},
		{"lang plus sdh", "movie.eng.sdh.srt", "el", "movie.el.sdh.srt"},
		{"lang plus forced", "movie.en.forced.srt", "el", "movie.el.forced.srt"},
		{"lang plus cc", "movie.en.cc.srt", "el", "movie.el.cc.srt"},
		{"hi marker not hindi", "movie.hi.srt", "el", "movie.el.hi.srt"},
		{"two markers keep order", "movie.en.forced.sdh.srt", "el", "movie.el.forced.sdh.srt"},
		{"marker case insensitive", "movie.en.SDH.srt", "el", "movie.el.SDH.srt"},
		{"marker only, no lang", "movie.forced.srt", "el", "movie.el.forced.srt"},
		{"hearing impaired", "movie.en.hearing.impaired.srt", "el", "movie.el.hearing.impaired.srt"},

		// P0 #3: sub/srt/ass/ssa are well-formed but unassigned subtags, so
		// language.Parse accepts them. The names lookup is what rejects them.
		{"sub segment", "movie.sub.srt", "el", "movie.el.sub.srt"},
		{"ass segment", "movie.ass.srt", "el", "movie.el.ass.srt"},

		// Nothing to replace: append.
		{"year", "movie.2024.srt", "el", "movie.2024.el.srt"},
		{"episode code", "s01e01.srt", "el", "s01e01.el.srt"},
		{"plain name", "movie.srt", "el", "movie.el.srt"},
		{"release name", "Sirat.2025.1080p.WEBRip.srt", "el", "Sirat.2025.1080p.WEBRip.el.srt"},

		// No extension at all.
		{"no extension", "movie", "el", "movie.el.srt"},
		{"no extension with dot dir", "/a.b/movie", "el", "/a.b/movie.el.srt"},
		{"unknown extension", "movie.2024", "el", "movie.2024.el.srt"},

		// Extension handling.
		{"uppercase extension", "movie.en.SRT", "el", "movie.el.SRT"},
		{"mixed case extension", "movie.en.Srt", "el", "movie.el.Srt"},
		{"vtt", "movie.en.vtt", "el", "movie.el.vtt"},
		{"ass extension", "movie.en.ass", "el", "movie.el.ass"},
		{"sub extension", "movie.en.sub", "el", "movie.el.sub"},

		// Directories are carried through verbatim, including "./".
		{"relative dot", "./movie.en.srt", "el", "./movie.el.srt"},
		{"absolute", "/subs/movie.en.srt", "el", "/subs/movie.el.srt"},
		{"nested", "a/b/movie.en.srt", "el", "a/b/movie.el.srt"},
		{"dot in directory", "/a.b/movie.en.srt", "el", "/a.b/movie.el.srt"},

		// The single-segment stem must survive: en.srt is a file called
		// "en", not a language with nothing in front of it.
		{"stem is only a language", "en.srt", "el", "en.el.srt"},
		{"stem is only a marker", "sdh.srt", "el", "sdh.el.srt"},

		// Script and region targets.
		{"traditional chinese target", "movie.en.srt", "zh-Hant", "movie.zh-Hant.srt"},
		{"brazilian target", "movie.en.srt", "pt-BR", "movie.pt-BR.srt"},

		// Idempotence: already-translated input derives to itself. The
		// caller detects that with SameFile and refuses to overwrite.
		{"already target", "movie.el.srt", "el", "movie.el.srt"},
		{"already target long code", "movie.ell.srt", "el", "movie.el.srt"},

		// A bare zh segment is still a language segment, so it gets
		// replaced even though we refuse zh as a target.
		{"zh segment replaced", "movie.zh.srt", "el", "movie.el.srt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Paths are written with "/" and translated to the host
			// separator, since DeriveOutputPath preserves the directory
			// prefix verbatim.
			in, want := native(tc.in), native(tc.want)

			target, err := Resolve(tc.target)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.target, err)
			}
			got, err := DeriveOutputPath(in, target)
			if err != nil {
				t.Fatalf("DeriveOutputPath(%q, %s): %v", in, tc.target, err)
			}
			if got != want {
				t.Errorf("DeriveOutputPath(%q, %s) = %q, want %q", in, tc.target, got, want)
			}
		})
	}
}

func TestDeriveOutputPathIdempotent(t *testing.T) {
	t.Parallel()

	// Running the derivation on its own output must be a fixed point;
	// otherwise a second run of the tool writes to a third filename and the
	// SameFile guard never fires.
	el, err := Resolve("greek")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"movie.en.srt", "movie.eng.sdh.srt", "movie.2024.srt", "s01e01.srt",
		"movie", "movie.en.forced.srt", "/a.b/movie.fre.srt",
	} {
		once, err := DeriveOutputPath(in, el)
		if err != nil {
			t.Fatalf("DeriveOutputPath(%q): %v", in, err)
		}
		twice, err := DeriveOutputPath(once, el)
		if err != nil {
			t.Fatalf("DeriveOutputPath(%q): %v", once, err)
		}
		if once != twice {
			t.Errorf("DeriveOutputPath not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestDeriveOutputPathErrors(t *testing.T) {
	t.Parallel()

	el, err := Resolve("el")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		in     string
		target Lang
		want   string
	}{
		{"empty stem", ".srt", el, "no filename"},
		{"empty stem with dir", "/subs/.srt", el, "no filename"},
		{"empty path", "", el, "empty input path"},
		{"stdin", "-", el, "stdin"},
		{"trailing separator", "subs" + string(filepath.Separator), el, "directory"},
		{"zero target", "movie.en.srt", Lang{}, "no target language"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DeriveOutputPath(tc.in, tc.target)
			if err == nil {
				t.Fatalf("DeriveOutputPath(%q) = %q, want error", tc.in, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("DeriveOutputPath(%q) error = %q, want it to mention %q", tc.in, err, tc.want)
			}
		})
	}
}

// TestExtIsDirectoryAware pins down the assumption that lets DeriveOutputPath
// skip any special handling for dots in parent directories.
func TestExtIsDirectoryAware(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"/a.b/movie", ""},
		{"a.b/movie", ""},
		{"/a.b/movie.srt", ".srt"},
		{"movie", ""},
		{".srt", ".srt"},
		{"movie.2024.srt", ".srt"},
	} {
		if got := filepath.Ext(tc.in); got != tc.want {
			t.Errorf("filepath.Ext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolvesAsLanguage(t *testing.T) {
	t.Parallel()

	yes := []string{"en", "eng", "el", "ell", "gre", "de", "ger", "fr", "fre", "pt", "nb", "sr", "zh", "EN"}
	no := []string{
		"sdh", "hi", "cc", "sub", "subs", "srt", "ass", "ssa", "vtt", "forced", "hoh", "dub",
		"2024", "s01e01", "1080p", "x", "", "movie", "qqq", "web",
	}

	for _, s := range yes {
		if !resolvesAsLanguage(s) {
			t.Errorf("resolvesAsLanguage(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if resolvesAsLanguage(s) {
			t.Errorf("resolvesAsLanguage(%q) = true, want false", s)
		}
	}
}

// native rewrites a slash-separated test path to the host separator.
func native(p string) string {
	return strings.ReplaceAll(p, "/", string(filepath.Separator))
}

func TestFromFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string // "" means no language should be found
	}{
		{"plain code", "movie.en.srt", "en"},
		{"three letter bibliographic", "movie.gre.srt", "el"},
		{"three letter terminological", "movie.ell.srt", "el"},
		{"uppercase", "movie.EN.srt", "en"},
		{"marker after the code", "movie.eng.sdh.srt", "en"},
		{"several markers", "movie.en.forced.hi.srt", "en"},
		{"full path", native("/tmp/films/movie.fr.srt"), "fr"},
		{"other extension", "movie.de.vtt", "de"},

		// The whole reason the marker table is peeled first: each of these
		// parses as a language on its own.
		{"bare sdh is a track type", "movie.sdh.srt", ""},
		{"bare hi is hearing impaired", "movie.hi.srt", ""},
		{"bare cc is a track type", "movie.cc.srt", ""},

		{"no language segment", "movie.srt", ""},
		{"year is not a language", "movie.2024.srt", ""},
		{"episode code is not a language", "s01e01.srt", ""},
		{"resolution is not a language", "movie.1080p.srt", ""},
		{"bare stem", "movie", ""},
		{"language alone would leave no stem", "en.srt", ""},
		{"empty", "", ""},
		{"stdin sentinel", "-", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := FromFilename(tt.in)
			if tt.want == "" {
				if ok {
					t.Errorf("FromFilename(%q) = %q, want no match", tt.in, got.Code)
				}
				return
			}
			if !ok {
				t.Fatalf("FromFilename(%q) found nothing, want %q", tt.in, tt.want)
			}
			if got.Code != tt.want {
				t.Errorf("FromFilename(%q) = %q, want %q", tt.in, got.Code, tt.want)
			}
		})
	}
}

// TestFromFilenameAgreesWithDerivation is the point of exporting FromFilename:
// detection and output-path derivation must classify a trailing segment the
// same way. If they disagree, movie.en.sdh.srt is read as Southern Kurdish by
// one and as English-plus-a-marker by the other.
func TestFromFilenameAgreesWithDerivation(t *testing.T) {
	t.Parallel()

	greek, err := Resolve("el")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, in := range []string{
		"movie.en.srt", "movie.eng.sdh.srt", "movie.sdh.srt", "movie.hi.srt",
		"movie.srt", "movie.2024.srt", "movie.en.forced.srt",
	} {
		out, err := DeriveOutputPath(in, greek)
		if err != nil {
			t.Fatalf("DeriveOutputPath(%q): %v", in, err)
		}
		// Whatever derivation produced must itself read back as Greek.
		got, ok := FromFilename(out)
		if !ok {
			t.Errorf("DeriveOutputPath(%q) = %q, which FromFilename does not recognise", in, out)
			continue
		}
		if got.Code != "el" {
			t.Errorf("DeriveOutputPath(%q) = %q, read back as %q, want el", in, out, got.Code)
		}
	}
}

func TestSidecarPath(t *testing.T) {
	t.Parallel()

	english, err := Resolve("eng")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	tests := []struct {
		name    string
		in      string
		lang    Lang
		markers []string
		want    string
	}{
		{name: "mkv", in: "movie.mkv", lang: english, want: "movie.en.srt"},
		{name: "webm", in: "clip.webm", lang: english, want: "clip.en.srt"},
		{name: "uppercase extension", in: "movie.MKV", lang: english, want: "movie.en.srt"},
		{name: "directory is kept verbatim", in: "./films/movie.mkv", lang: english, want: "./films/movie.en.srt"},
		{name: "absolute path", in: "/srv/media/movie.mkv", lang: english, want: "/srv/media/movie.en.srt"},
		// Only container extensions are stripped. A dotted release name
		// is part of the stem, and removing ".2160p" would be inventing a
		// filename the user did not have.
		{name: "dotted release name", in: "Movie.Title.1991.2160p.mkv", lang: english, want: "Movie.Title.1991.2160p.en.srt"},
		{name: "unknown extension is part of the stem", in: "movie.avi", lang: english, want: "movie.avi.en.srt"},
		{name: "no extension", in: "movie", lang: english, want: "movie.en.srt"},
		// The forced and the full track of one language would otherwise
		// derive the same name and one would overwrite the other.
		{name: "forced", in: "movie.mkv", lang: english, markers: []string{"forced"}, want: "movie.en.forced.srt"},
		{name: "sdh", in: "movie.mkv", lang: english, markers: []string{"sdh"}, want: "movie.en.sdh.srt"},
		{name: "both markers", in: "movie.mkv", lang: english, markers: []string{"forced", "sdh"}, want: "movie.en.forced.sdh.srt"},
		// An untagged track has no code to put in the name. Asserting one
		// would be worse than leaving it out.
		{name: "unknown language", in: "movie.mkv", lang: Lang{}, want: "movie.srt"},
		{name: "unknown language with a marker", in: "movie.mkv", lang: Lang{}, markers: []string{"forced"}, want: "movie.forced.srt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SidecarPath(tt.in, tt.lang, tt.markers...)
			if err != nil {
				t.Fatalf("SidecarPath(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("SidecarPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSidecarPathRejects(t *testing.T) {
	t.Parallel()

	english, err := Resolve("eng")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, in := range []string{"", "-", "films/"} {
		if got, err := SidecarPath(in, english); err == nil {
			t.Errorf("SidecarPath(%q) = %q, want an error", in, got)
		}
	}
}

// A sidecar name is the input to translate, so the two derivations have to
// agree: movie.en.forced.srt must become movie.el.forced.srt, with "forced" left
// where it is rather than read as the language.
func TestSidecarPathFeedsDeriveOutputPath(t *testing.T) {
	t.Parallel()

	english, err := Resolve("eng")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	greek, err := Resolve("el")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	tests := []struct {
		markers []string
		want    string
	}{
		{nil, "movie.el.srt"},
		{[]string{"forced"}, "movie.el.forced.srt"},
		{[]string{"sdh"}, "movie.el.sdh.srt"},
	}
	for _, tt := range tests {
		sidecar, err := SidecarPath("movie.mkv", english, tt.markers...)
		if err != nil {
			t.Fatalf("SidecarPath: %v", err)
		}
		got, err := DeriveOutputPath(sidecar, greek)
		if err != nil {
			t.Fatalf("DeriveOutputPath(%q): %v", sidecar, err)
		}
		if got != tt.want {
			t.Errorf("%q -> %q, want %q", sidecar, got, tt.want)
		}
	}
}
