package translate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
)

const briefJSON = `{
  "characters": [
    {"name": "Luis", "rendered": "Λουίς", "gender": "male", "note": "the father"},
    {"name": "Esteban", "rendered": "Εστεμπάν", "gender": "male"}
  ],
  "register": [
    {"from": "Luis", "to": "Esteban", "form": "εσύ", "reason": "peers on the road"}
  ],
  "glossary": [
    {"term": "the rave", "rendered": "το πάρτι", "note": "always definite"}
  ],
  "tone": "Terse, sun-blasted, profane.",
  "setting": "Southern Morocco, present day."
}`

func TestBriefIsInjectedIntoEveryBatch(t *testing.T) {
	t.Parallel()

	in := makeCues(30)
	p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
		if req.Stage == "brief" {
			return llm.Response{Content: briefJSON}, nil
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	o := opts(p, &warns)
	o.Brief = true
	o.BatchSize = 10
	o.Concurrency = 1

	res, err := Run(context.Background(), in, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if res.Brief == nil {
		t.Fatal("Brief is nil")
	}
	if len(res.Brief.Characters) != 2 || res.Brief.Characters[0].Rendered != "Λουίς" {
		t.Errorf("brief = %+v", res.Brief)
	}

	reqs := p.requests()
	if reqs[0].Stage != "brief" {
		t.Fatalf("first call stage = %q, want brief", reqs[0].Stage)
	}
	if len(reqs) != 4 {
		t.Fatalf("calls = %d, want 4 (one brief plus three batches)", len(reqs))
	}
	for i, req := range reqs[1:] {
		sys := systemMessage(req)
		for _, want := range []string{"FILM BRIEF", "Λουίς", "εσύ", "το πάρτι", "Terse"} {
			if !strings.Contains(sys, want) {
				t.Errorf("batch %d system prompt is missing %q", i, want)
			}
		}
	}
}

// A failed brief costs quality, never the run.
func TestBriefFailureDegradesGracefully(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp func() (llm.Response, error)
		warn string
	}{
		{"call fails", func() (llm.Response, error) {
			return llm.Response{}, errors.New("upstream 500")
		}, "unavailable"},
		{"unparseable json", func() (llm.Response, error) {
			return llm.Response{Content: "no idea, sorry"}, nil
		}, "unavailable"},
		{"empty brief", func() (llm.Response, error) {
			return llm.Response{Content: `{"characters":[],"register":[],"glossary":[],"tone":"","setting":""}`}, nil
		}, "nothing usable"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := makeCues(20)
			p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
				if req.Stage == "brief" {
					return tc.resp()
				}
				return llm.Response{Content: reply(req, prefix("EL:"))}, nil
			}}

			var warns []string
			o := opts(p, &warns)
			o.Brief = true

			res, err := Run(context.Background(), in, o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertShape(t, in, res.Cues)

			if res.Brief != nil {
				t.Errorf("Brief = %+v, want nil", res.Brief)
			}
			if !hasWarning(warns, tc.warn) {
				t.Errorf("warnings = %q, want one mentioning %q", warns, tc.warn)
			}
			if res.Stats.Untranslated != 0 {
				t.Errorf("untranslated = %d: a missing brief must not cost a cue", res.Stats.Untranslated)
			}
			if !strings.Contains(systemMessage(firstBatch(t, p)), "CUE INTEGRITY") {
				t.Error("the system prompt lost its rules along with the brief")
			}
		})
	}
}

func TestBriefSkipped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cues  int
		brief bool
	}{
		{"disabled", 40, false},
		{"file too small", minBriefCues - 1, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := echoing(prefix("EL:"))
			var warns []string
			o := opts(p, &warns)
			o.Brief = tc.brief

			res, err := Run(context.Background(), makeCues(tc.cues), o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Brief != nil {
				t.Errorf("Brief = %+v, want nil", res.Brief)
			}
			for _, req := range p.requests() {
				if req.Stage == "brief" {
					t.Fatal("pass 0 ran when it should have been skipped")
				}
			}
		})
	}
}

func TestBriefPrompt(t *testing.T) {
	t.Parallel()

	if got := (*Brief)(nil).prompt(); got != "" {
		t.Errorf("nil brief rendered %q", got)
	}
	empty := &Brief{}
	if got := empty.prompt(); got != "" {
		t.Errorf("empty brief rendered %q", got)
	}

	b := &Brief{
		Characters: []BriefCharacter{{Name: "Ana", Gender: "female"}},
		Tone:       "Dry.",
	}
	got := b.prompt()
	for _, want := range []string{"FILM BRIEF", "Ana", "female", "Dry."} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
	// A character with no rendering must still appear, with a visible gap
	// rather than an empty arrow.
	if !strings.Contains(got, "Ana → —") {
		t.Errorf("missing rendering not marked:\n%s", got)
	}
}

func TestBriefSourceTruncates(t *testing.T) {
	t.Parallel()

	long := makeCues(3)
	long[1].Lines = []string{strings.Repeat("x", briefCharBudget)}

	text, truncated := briefSource(long)
	if !truncated {
		t.Fatal("briefSource did not report truncation")
	}
	if len(text) > briefCharBudget {
		t.Errorf("briefSource returned %d characters, above the %d budget", len(text), briefCharBudget)
	}

	if _, truncated := briefSource(makeCues(3)); truncated {
		t.Error("a short file must not be reported as truncated")
	}
}

// The system prompt has to carry the rules, the target language and the
// language-specific addendum, or none of the rest matters.
func TestSystemPrompt(t *testing.T) {
	t.Parallel()

	got := systemPrompt(english(), greek(), nil)
	for _, want := range []string{
		"CUE INTEGRITY",
		"DO NOT TRANSLATE",
		"42 characters",
		"English subtitles",
		"Greek (Ελληνικά)",
		"GREEK CONVENTIONS",
		`";"`,
		"17 characters per second",
		"εσύ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}

	// An unknown source language must not be invented — the zero Lang is a
	// supported, ordinary case.
	if s := systemPrompt(lang.Lang{}, greek(), nil); !strings.Contains(s, "translate subtitles into Greek") {
		t.Errorf("zero source language rendered %q", s)
	}
}

func TestTargetAddendum(t *testing.T) {
	t.Parallel()

	if got := targetAddendum(greek()); !strings.Contains(got, "GREEK CONVENTIONS") {
		t.Errorf("no Greek addendum: %q", got)
	}
	// A language with no addendum yet must simply get none.
	if got := targetAddendum(english()); got != "" {
		t.Errorf("English addendum = %q, want empty", got)
	}
}

// firstBatch returns the first batch request, skipping the brief and any repair
// round-trip llm.CompleteJSON made on its behalf.
func firstBatch(t *testing.T, p *fakeProvider) llm.Request {
	t.Helper()
	for _, req := range p.requests() {
		if req.Stage == "batch" {
			return req
		}
	}
	t.Fatal("no batch request was made")
	return llm.Request{}
}

// The addendum falls back to the base language, so a regional target still gets
// the conventions of the language it is a variant of.
func TestTargetAddendumFallsBackToTheBaseLanguage(t *testing.T) {
	t.Parallel()

	regional := lang.Lang{Code: "el-GR", English: "Greek (Greece)"}
	if got := targetAddendum(regional); !strings.Contains(got, "GREEK CONVENTIONS") {
		t.Errorf("el-GR got no Greek addendum: %q", got)
	}
	if got := targetAddendum(lang.Lang{Code: "xx-YY"}); got != "" {
		t.Errorf("unknown regional target got %q", got)
	}
}
