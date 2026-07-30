package lang

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/text/language"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		code    string
		english string
		native  string
	}{
		// The five spellings of Greek that must all collapse to "el";
		// anything else and the output filename stops being idempotent.
		{"iso639_1", "el", "el", "Greek", "Ελληνικά"},
		{"iso639_2_t", "ell", "el", "Greek", "Ελληνικά"},
		{"iso639_2_b", "gre", "el", "Greek", "Ελληνικά"},
		{"uppercase code", "EL", "el", "Greek", "Ελληνικά"},
		{"english name lower", "greek", "el", "Greek", "Ελληνικά"},
		{"english name title", "Greek", "el", "Greek", "Ελληνικά"},
		{"english name spaced", "  Greek  ", "el", "Greek", "Ελληνικά"},
		{"native name", "Ελληνικά", "el", "Greek", "Ελληνικά"},
		{"tag with region", "el-GR", "el", "Greek", "Ελληνικά"},

		// The other half of the B/T split.
		{"german t", "de", "de", "German", "Deutsch"},
		{"german b", "ger", "de", "German", "Deutsch"},
		{"german deu", "deu", "de", "German", "Deutsch"},
		{"german name", "german", "de", "German", "Deutsch"},
		{"french t", "fr", "fr", "French", "Français"},
		{"french b", "fre", "fr", "French", "Français"},
		{"french name", "FRENCH", "fr", "French", "Français"},

		// Norwegian: three codes that are all legitimate and must stay
		// distinct rather than being folded into each other.
		{"bokmal", "nb", "nb", "Norwegian Bokmål", "Norsk bokmål"},
		{"bokmal name", "bokmal", "nb", "Norwegian Bokmål", "Norsk bokmål"},
		{"nynorsk", "nn", "nn", "Norwegian Nynorsk", "Norsk nynorsk"},
		{"norwegian", "no", "no", "Norwegian", "Norsk"},
		{"norwegian nor", "nor", "no", "Norwegian", "Norsk"},

		// Serbo-Croatian: script matters, and "sh" is a retired tag that
		// x/text canonicalises to sr-Latn.
		{"serbian", "sr", "sr", "Serbian", "Српски"},
		{"serbian latin", "sr-Latn", "sr-Latn", "Serbian (Latin)", "Srpski"},
		{"serbo-croatian", "sh", "sr-Latn", "Serbian (Latin)", "Srpski"},
		{"croatian", "hr", "hr", "Croatian", "Hrvatski"},
		{"bosnian", "bos", "bs", "Bosnian", "Bosanski"},

		// Script and region variants that must survive rather than being
		// collapsed to the base language.
		{"simplified", "zh-Hans", "zh-Hans", "Chinese (Simplified)", "简体中文"},
		{"traditional", "zh-Hant", "zh-Hant", "Chinese (Traditional)", "繁體中文"},
		{"traditional with region", "zh-Hant-TW", "zh-Hant", "Chinese (Traditional)", "繁體中文"},
		{"simplified alias", "simplified chinese", "zh-Hans", "Chinese (Simplified)", "简体中文"},
		{"brazilian", "pt-BR", "pt-BR", "Brazilian Portuguese", "Português brasileiro"},
		{"brazilian lowercase", "pt-br", "pt-BR", "Brazilian Portuguese", "Português brasileiro"},
		{"portuguese", "pt", "pt", "Portuguese", "Português"},

		// Deprecated codes that x/text silently modernises.
		{"hebrew deprecated", "iw", "he", "Hebrew", "עברית"},
		{"indonesian deprecated", "in", "id", "Indonesian", "Bahasa Indonesia"},
		{"tagalog", "tl", "fil", "Filipino", "Filipino"},

		{"english", "en", "en", "English", "English"},
		{"spanish 3letter", "spa", "es", "Spanish", "Español"},
		{"underscore locale", "pt_BR", "pt-BR", "Brazilian Portuguese", "Português brasileiro"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Resolve(tc.in)
			if err != nil {
				t.Fatalf("Resolve(%q): unexpected error: %v", tc.in, err)
			}
			if got.Code != tc.code {
				t.Errorf("Resolve(%q).Code = %q, want %q", tc.in, got.Code, tc.code)
			}
			if got.English != tc.english {
				t.Errorf("Resolve(%q).English = %q, want %q", tc.in, got.English, tc.english)
			}
			if got.Native != tc.native {
				t.Errorf("Resolve(%q).Native = %q, want %q", tc.in, got.Native, tc.native)
			}
			if got.Tag.String() != tc.code {
				t.Errorf("Resolve(%q).Tag = %q, want %q", tc.in, got.Tag, tc.code)
			}
			if got.Zero() {
				t.Errorf("Resolve(%q).Zero() = true", tc.in)
			}
		})
	}
}

func TestResolveIdempotent(t *testing.T) {
	t.Parallel()

	// Feeding a resolved Code back into Resolve must be a fixed point,
	// otherwise re-running the tool on its own output renames the file.
	for _, in := range []string{"el", "ell", "gre", "greek", "Greek", "EL", "el-GR", "ger", "fre", "sh", "tl", "zh-Hant-TW"} {
		first, err := Resolve(in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", in, err)
		}
		second, err := Resolve(first.Code)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", first.Code, err)
		}
		if first != second {
			t.Errorf("Resolve(%q) not idempotent: %+v then %+v", in, first, second)
		}
	}
}

func TestResolveAmbiguous(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"zh", "chi", "zho", "chinese", "Chinese", "中文"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			_, err := Resolve(in)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("Resolve(%q) error = %v, want ErrAmbiguous", in, err)
			}
			// The message must name the way out, not just the problem.
			for _, want := range []string{"zh-Hans", "zh-Hant"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Resolve(%q) error %q does not mention %q", in, err, want)
				}
			}
		})
	}
}

func TestResolveUnknown(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"   ",
		"klingon",
		"greekk",
		"srt",  // parses as a tag, has no name
		"ass",  // ditto
		"ssa",  // ditto
		"sub",  // ditto
		"qqq",  // well-formed, unassigned
		"x-el", // private use
	}

	for _, in := range tests {
		t.Run("in="+in, func(t *testing.T) {
			t.Parallel()

			got, err := Resolve(in)
			if !errors.Is(err, ErrUnknown) {
				t.Fatalf("Resolve(%q) = %+v, %v; want ErrUnknown", in, got, err)
			}
			if !got.Zero() {
				t.Errorf("Resolve(%q) returned non-zero Lang %+v on error", in, got)
			}
		})
	}
}

// TestNamesTableConsistent guards the reverse index. Because it is built by
// ranging over a map, a duplicate English or native name would make -ol
// greek resolve to a different language depending on the run.
func TestNamesTableConsistent(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for key, n := range names {
		tag, err := language.Parse(key)
		if err != nil {
			t.Errorf("names key %q does not parse: %v", key, err)
			continue
		}
		if tag.String() != key {
			t.Errorf("names key %q is not canonical; language.Parse gives %q", key, tag)
		}
		if n.English == "" || n.Native == "" {
			t.Errorf("names[%q] has an empty name: %+v", key, n)
		}
		for _, name := range []string{n.English, n.Native} {
			folded := foldName(name)
			if prev, dup := seen[folded]; dup && prev != key {
				t.Errorf("name %q is claimed by both %q and %q", name, prev, key)
			}
			seen[folded] = key
		}
	}

	for alias, key := range aliases {
		if !known(key) {
			t.Errorf("alias %q points at unknown key %q", alias, key)
		}
		if foldName(alias) != alias {
			t.Errorf("alias %q is not already folded (want %q)", alias, foldName(alias))
		}
	}
	for name, base := range ambiguousNames {
		if !ambiguousBases[base] {
			t.Errorf("ambiguousNames[%q] = %q, which is not in ambiguousBases", name, base)
		}
		if _, clash := nameIndex()[foldName(name)]; clash {
			t.Errorf("ambiguous name %q is also a resolvable name", name)
		}
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	el, err := Resolve("el")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := el.String(), "Greek (el)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Lang{}).String(), "unknown"; got != want {
		t.Errorf("zero String() = %q, want %q", got, want)
	}
}

func TestSameFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "movie.el.srt")
	if err := os.WriteFile(real, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.srt")
	if err := os.WriteFile(other, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// "./x" versus "x": equal files, unequal strings. This is the case a
	// naive string compare misses.
	dotted := filepath.Join(dir, ".", "movie.el.srt")
	weird := dir + string(filepath.Separator) + "." + string(filepath.Separator) + "movie.el.srt"

	link := filepath.Join(dir, "link.srt")
	hard := filepath.Join(dir, "hard.srt")
	haveLink, haveHard := true, true
	if err := os.Symlink(real, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatalf("symlink: %v", err)
		}
		haveLink = false
	}
	if err := os.Link(real, hard); err != nil {
		haveHard = false
	}

	tests := []struct {
		name string
		a, b string
		want bool
		skip bool
	}{
		{name: "identical", a: real, b: real, want: true},
		{name: "dot segment", a: real, b: dotted, want: true},
		{name: "unclean dot segment", a: real, b: weird, want: true},
		{name: "different files", a: real, b: other, want: false},
		{name: "symlink", a: real, b: link, want: true, skip: !haveLink},
		{name: "hardlink", a: real, b: hard, want: true, skip: !haveHard},
		{name: "missing b", a: real, b: filepath.Join(dir, "nope.srt"), want: false},
		{name: "missing a", a: filepath.Join(dir, "nope.srt"), b: real, want: false},
		{name: "both missing", a: filepath.Join(dir, "a"), b: filepath.Join(dir, "b"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.skip {
				t.Skip("unsupported on this platform")
			}
			got, err := SameFile(tc.a, tc.b)
			if err != nil {
				t.Fatalf("SameFile(%q, %q): %v", tc.a, tc.b, err)
			}
			if got != tc.want {
				t.Errorf("SameFile(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
