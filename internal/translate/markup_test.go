package translate

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// ASS override blocks never reach the model, and come back exactly where they
// were.
func TestRunASSOverridesStrippedAndRestored(t *testing.T) {
	t.Parallel()

	in := []srt.Cue{
		cue("1", 0, 2000, `{\an8}Top of frame`),
		cue("2", 2000, 4000, `{\i1}Whispered{\i0}`),
		cue("3", 4000, 6000, `{\pos(120,400)}Left`, `Right{\r}`),
	}
	p := echoing(prefix("EL:"))
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	// The model saw plain text.
	for _, c := range parseRequest(p.requests()[0]) {
		for _, line := range c.lines {
			if strings.Contains(line, `{\`) {
				t.Errorf("an ASS block reached the model: %q", line)
			}
		}
	}

	want := [][]string{
		{`{\an8}EL:Top of frame`},
		{`{\i1}EL:Whispered{\i0}`},
		{`{\pos(120,400)}EL:Left`, `EL:Right{\r}`},
	}
	for i, w := range want {
		if !slices.Equal(res.Cues[i].Lines, w) {
			t.Errorf("cue %d: %q, want %q", i, res.Cues[i].Lines, w)
		}
	}
	if res.Stats.Untranslated != 0 {
		t.Errorf("untranslated = %d, want 0", res.Stats.Untranslated)
	}
}

// HTML-ish styling goes to the model raw, because it is in-distribution and
// models return it correctly. When they do not, the cue falls back.
func TestRunHTMLMarkup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		transform    func(string) string
		wantLine     string
		untranslated int
		warning      string
	}{
		{
			name:      "tags survive",
			transform: func(s string) string { return strings.ReplaceAll(s, "Hello", "Γεια") },
			wantLine:  "<i>Γεια</i> there",
		},
		{
			name:      "attribute case change is not a mismatch",
			transform: func(s string) string { return strings.ReplaceAll(s, "Hello", "Γεια") },
			wantLine:  "<i>Γεια</i> there",
		},
		{
			name:         "dropped tag falls back",
			transform:    func(s string) string { return strings.NewReplacer("<i>", "", "</i>", "").Replace(s) },
			wantLine:     "<i>Hello</i> there",
			untranslated: 1,
			warning:      "lost or invented markup",
		},
		{
			name:         "invented tag falls back",
			transform:    func(s string) string { return s + "<b>!</b>" },
			wantLine:     "<i>Hello</i> there",
			untranslated: 1,
			warning:      "lost or invented markup",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := []srt.Cue{cue("1", 0, 2000, "<i>Hello</i> there")}
			p := echoing(tc.transform)
			var warns []string
			res, err := Run(context.Background(), in, opts(p, &warns))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertShape(t, in, res.Cues)

			if got := res.Cues[0].Lines[0]; got != tc.wantLine {
				t.Errorf("line = %q, want %q", got, tc.wantLine)
			}
			if res.Stats.Untranslated != tc.untranslated {
				t.Errorf("untranslated = %d, want %d", res.Stats.Untranslated, tc.untranslated)
			}
			if tc.warning != "" && !hasWarning(warns, tc.warning) {
				t.Errorf("warnings = %q, want one mentioning %q", warns, tc.warning)
			}
			// Whatever happened, the tag itself was never masked.
			if !strings.Contains(userMessage(p.requests()[0]), "<i>Hello</i>") {
				t.Error("markup did not reach the model raw")
			}
		})
	}
}

// Leading and trailing whitespace is load-bearing in real subtitle files and no
// model preserves it, so it is captured and re-attached here.
func TestRunWhitespaceIsReattached(t *testing.T) {
	t.Parallel()

	in := []srt.Cue{cue("1", 0, 2000, "    Indented line", "Trailing space  ")}
	p := echoing(prefix("EL:"))
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The model saw the text without the padding...
	got := parseRequest(p.requests()[0])[0]
	if !slices.Equal(got.lines, []string{"Indented line", "Trailing space"}) {
		t.Errorf("model saw %q", got.lines)
	}
	// ...and the padding came back.
	want := []string{"    EL:Indented line", "EL:Trailing space  "}
	if !slices.Equal(res.Cues[0].Lines, want) {
		t.Errorf("lines = %q, want %q", res.Cues[0].Lines, want)
	}
}

func TestSplitLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		in                   string
		core                 string
		leadWS, trailWS      string
		lead, mid, trail     []string
		rebuildWith, rebuilt string
		lossy                bool // an exact round trip is not possible
	}{
		{
			name: "plain", in: "Hello", core: "Hello",
			rebuildWith: "Γεια", rebuilt: "Γεια",
		},
		{
			name: "leading ass", in: `{\an8}Hello`, core: "Hello",
			lead: []string{`{\an8}`}, rebuildWith: "Γεια", rebuilt: `{\an8}Γεια`,
		},
		{
			name: "ass after indentation", in: `  {\an8}Hello `, core: "Hello",
			leadWS: "  ", trailWS: " ", lead: []string{`{\an8}`},
			rebuildWith: "Γεια", rebuilt: `  {\an8}Γεια `,
		},
		{
			name: "wrapping ass", in: `{\i1}Hello{\i0}`, core: "Hello",
			lead: []string{`{\i1}`}, trail: []string{`{\i0}`},
			rebuildWith: "Γεια", rebuilt: `{\i1}Γεια{\i0}`,
		},
		{
			name: "interior ass is appended", in: `Hello {\b1}world`, core: `Hello world`,
			mid:         []string{`{\b1}`},
			rebuildWith: "Γεια σου κόσμε", rebuilt: `Γεια σου κόσμε{\b1}`, lossy: true,
		},
		{
			name: "brace without backslash is dialogue", in: "{laughs} Hi", core: "{laughs} Hi",
			rebuildWith: "{laughs} Γεια", rebuilt: "{laughs} Γεια",
		},
		{
			name: "unterminated block is text", in: `{\an8 Hello`, core: `{\an8 Hello`,
			rebuildWith: "x", rebuilt: "x",
		},
		{
			name: "whitespace only", in: "   ", leadWS: "   ",
			rebuildWith: "", rebuilt: "   ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := splitLine(tc.in)
			if p.core != tc.core {
				t.Errorf("core = %q, want %q", p.core, tc.core)
			}
			if p.leadWS != tc.leadWS || p.trailWS != tc.trailWS {
				t.Errorf("whitespace = %q/%q, want %q/%q", p.leadWS, p.trailWS, tc.leadWS, tc.trailWS)
			}
			if !slices.Equal(p.lead, tc.lead) || !slices.Equal(p.mid, tc.mid) || !slices.Equal(p.trail, tc.trail) {
				t.Errorf("blocks = %q/%q/%q, want %q/%q/%q", p.lead, p.mid, p.trail, tc.lead, tc.mid, tc.trail)
			}
			if got := p.rebuild(tc.rebuildWith); got != tc.rebuilt {
				t.Errorf("rebuild = %q, want %q", got, tc.rebuilt)
			}
			// The identity round trip must be exact for every input except
			// the documented one: a block that sat inside the text has no
			// faithful position once the words have changed.
			if got := p.rebuild(p.core); !tc.lossy && got != tc.in {
				t.Errorf("round trip = %q, want %q", got, tc.in)
			}
		})
	}
}

func TestTagMultiset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want map[string]int
	}{
		{"none", "plain text", nil},
		{"italic", "<i>a</i>", map[string]int{"i": 1, "/i": 1}},
		{"repeated", "<i>a</i> <i>b</i>", map[string]int{"i": 2, "/i": 2}},
		{"font with attributes", `<font color="#ff0000">x</font>`, map[string]int{"font": 1, "/font": 1}},
		{"uppercase", "<I>a</I>", map[string]int{"i": 1, "/i": 1}},
		{"self closing", "a<br/>b", map[string]int{"br": 1}},
		// The whole reason the tag name set is closed.
		{"arithmetic is not markup", "if x < 10 > 3 then", nil},
		{"unknown tag", "<script>x</script>", nil},
		{"bare angle brackets", "a < b and c > d", nil},
		{"unterminated", "<i a", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tagMultiset(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("tags = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("tags[%q] = %d, want %d (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestSameTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src, dst []string
		want     bool
	}{
		{"identical", []string{"<i>a</i>"}, []string{"<i>β</i>"}, true},
		// The multiset is per cue, not per line: a translation that reorders
		// clauses may legitimately move a tag to the other line.
		{"moved between lines", []string{"<i>a", "b</i>"}, []string{"<i>β</i>", "γ"}, true},
		{"attributes differ", []string{`<font color="#FFF">a</font>`}, []string{`<font color="#fff">β</font>`}, true},
		{"one dropped", []string{"<i>a</i>"}, []string{"β"}, false},
		{"one added", []string{"a"}, []string{"<b>β</b>"}, false},
		{"none either side", []string{"a"}, []string{"β"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sameTags(tc.src, tc.dst); got != tc.want {
				t.Errorf("sameTags = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBalancedSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		n    int
		want []string
	}{
		{"one line", "a b c", 1, []string{"a b c"}},
		{"even", "aaa bbb ccc ddd", 2, []string{"aaa bbb", "ccc ddd"}},
		{"prefers a shorter first line", "aa bb cccccc", 2, []string{"aa bb", "cccccc"}},
		{"three", "one two three four five six", 3, []string{"one two", "three four", "five six"}},
		{"fewer words than lines", "solo", 3, []string{"", "", "solo"}},
		{"empty", "", 2, []string{"", ""}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := balancedSplit(tc.text, tc.n)
			if len(got) != tc.n {
				t.Fatalf("got %d lines, want exactly %d: %q", len(got), tc.n, got)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("split = %q, want %q", got, tc.want)
			}
			// No word may be lost or duplicated.
			if strings.Join(strings.Fields(strings.Join(got, " ")), " ") != strings.Join(strings.Fields(tc.text), " ") {
				t.Errorf("split changed the text: %q from %q", got, tc.text)
			}
		})
	}
}

// balancedSplit is the recovery path for a protocol violation, so it must be
// total: any text, any n, exactly n lines back, never a panic.
func TestBalancedSplitIsTotal(t *testing.T) {
	t.Parallel()

	texts := []string{"", " ", "one", "one two", strings.Repeat("word ", 40), "  spaced   out  "}
	for _, text := range texts {
		for n := 1; n <= 5; n++ {
			got := balancedSplit(text, n)
			if len(got) != n {
				t.Errorf("balancedSplit(%q, %d) returned %d lines: %q", text, n, len(got), got)
			}
		}
	}
}

// The interior-override case is rare and lossy, so it is announced.
func TestPrepareWarnsAboutInteriorOverrides(t *testing.T) {
	t.Parallel()

	var warns []string
	r := newRunner(opts(nil, &warns))
	prep := r.prepare([]srt.Cue{cue("1", 0, 1000, `Hello {\b1}world`)}, batchRange{0, 1})
	if len(prep) != 1 {
		t.Fatalf("prepared %d cues, want 1", len(prep))
	}
	if !hasWarning(warns, "ASS override block") {
		t.Errorf("warnings = %q", warns)
	}
}

func TestTagList(t *testing.T) {
	t.Parallel()

	if got := tagList([]string{"plain"}); got != "none" {
		t.Errorf("tagList = %q, want %q", got, "none")
	}
	if got := tagList([]string{"<i>a</i><i>b</i>"}); got != "/i×2 i×2" {
		t.Errorf("tagList = %q", got)
	}
}
