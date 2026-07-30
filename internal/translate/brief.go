package translate

import (
	"context"
	"fmt"
	"strings"

	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// Pass 0: one call over the whole file before any batch is translated.
//
// A feature-length subtitle file is a few thousand tokens — 38 KB of .srt is
// about 5k tokens — so the whole thing fits in one context on every model worth
// using. That one cheap call buys the three decisions that a batch cannot make
// for itself and that must not be made twice:
//
//   - how each character's name is rendered, and their gender (Greek inflects
//     adjectives and participles for the gender of the person addressed, so
//     "you're tired" is not translatable without knowing who is speaking to
//     whom);
//   - the second-person register for each pair of characters (εσύ or εσείς),
//     which is invisible in the English source and glaring in the Greek output
//     when it flips halfway through a scene;
//   - the recurring terms that have to be rendered the same way every time.
//
// The result is injected verbatim into every batch's system prompt, which is
// what makes it compatible with concurrency: batches never need to see each
// other's output, only this.
//
// Every failure here degrades to a warning. A missing brief costs quality; a
// failed run costs the whole translation.

// minBriefCues is the size below which the brief is skipped. A file this short
// is a trailer, a sample, or a single scene: there is no cast to be consistent
// about and the call would be pure overhead.
const minBriefCues = 12

// briefCharBudget caps how much source text is sent. It is generous enough for
// a three-hour film and small enough that a pathological input cannot turn pass
// 0 into the most expensive call of the run.
// briefCharBudget bounds the dialogue sent to pass 0.
//
// It is far smaller than a film's script on purpose, and the reason is
// measurable: pass 0's cost is dominated by reasoning over the dialogue, not by
// the shape of the reply. On a 734-cue episode against deepseek-v4-flash,
// trimming the requested fields cut the generated content by two thirds but the
// reasoning by only a sixth — 14.3k tokens of it remained, on 7.8k tokens of
// input. The input is the lever.
//
// The brief needs enough dialogue to place the cast, the register between them
// and the recurring terms. It does not need every line, and beyond a point more
// text buys nothing but latency on the one call that gates the whole run.
const briefCharBudget = 16_000

// Brief is the pass-0 analysis of the whole file.
type Brief struct {
	Characters []BriefCharacter `json:"characters"`
	Register   []BriefRegister  `json:"register"`
	Glossary   []BriefTerm      `json:"glossary"`
	Tone       string           `json:"tone"`
	Setting    string           `json:"setting"`
}

// BriefCharacter is one speaking part.
type BriefCharacter struct {
	Name     string `json:"name"`
	Rendered string `json:"rendered"`
	Gender   string `json:"gender"`
}

// BriefRegister is the second-person form one character uses towards another.
type BriefRegister struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Form   string `json:"form"`
	Reason string `json:"reason"`
}

// BriefTerm is one recurring term and its fixed rendering.
type BriefTerm struct {
	Term     string `json:"term"`
	Rendered string `json:"rendered"`
}

// empty reports whether the brief carries nothing worth injecting.
func (b *Brief) empty() bool {
	return b == nil || (len(b.Characters) == 0 && len(b.Register) == 0 &&
		len(b.Glossary) == 0 && b.Tone == "" && b.Setting == "")
}

// prompt renders the brief for injection into the system message.
func (b *Brief) prompt() string {
	if b.empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("FILM BRIEF — decided once for the whole file; follow it exactly and do not re-decide per scene:")
	if b.Setting != "" {
		fmt.Fprintf(&sb, "\nSetting: %s", b.Setting)
	}
	if b.Tone != "" {
		fmt.Fprintf(&sb, "\nTone: %s", b.Tone)
	}
	if len(b.Characters) > 0 {
		sb.WriteString("\nCharacters (name → rendering, gender):")
		for _, c := range b.Characters {
			fmt.Fprintf(&sb, "\n  %s → %s", c.Name, orDash(c.Rendered))
			if c.Gender != "" {
				fmt.Fprintf(&sb, " (%s)", c.Gender)
			}
		}
	}
	if len(b.Register) > 0 {
		sb.WriteString("\nSecond-person register (use exactly this form, never switch):")
		for _, r := range b.Register {
			fmt.Fprintf(&sb, "\n  %s → %s: %s", r.From, r.To, r.Form)
			if r.Reason != "" {
				fmt.Fprintf(&sb, " (%s)", r.Reason)
			}
		}
	}
	if len(b.Glossary) > 0 {
		sb.WriteString("\nGlossary (fixed renderings):")
		for _, g := range b.Glossary {
			fmt.Fprintf(&sb, "\n  %s → %s", g.Term, orDash(g.Rendered))
		}
	}
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// briefSchema is embedded in the prompt by the llm client (OpenCode Zen rejects
// response_format: json_schema outright, so the schema goes in as text).
//
// Every field here is paid for twice over. Embedding the schema instructs the
// model to "include every property", so an optional-looking field is generated
// for every array element, and each one is reasoned about first. Measured on a
// 734-cue episode against deepseek-v4-flash: adding the schema message took one
// call from 65s and 9.2k reasoning tokens to 134s and 17.2k. Free-text "note"
// fields on characters and glossary terms were the bulk of that and bought
// nothing the translator needed, so they are gone. Keep this shape lean.
var briefSchema = &llm.JSONSchema{
	Name: "subtitle_brief",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"characters": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"rendered": map[string]any{"type": "string"},
						"gender":   map[string]any{"type": "string"},
					},
				},
			},
			"register": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"from":   map[string]any{"type": "string"},
						"to":     map[string]any{"type": "string"},
						"form":   map[string]any{"type": "string"},
						"reason": map[string]any{"type": "string"},
					},
				},
			},
			"glossary": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"term":     map[string]any{"type": "string"},
						"rendered": map[string]any{"type": "string"},
					},
				},
			},
			"tone":    map[string]any{"type": "string"},
			"setting": map[string]any{"type": "string"},
		},
		"required": []any{"characters", "register", "glossary", "tone", "setting"},
	},
}

// makeBrief runs pass 0. It never returns an error: a brief that could not be
// produced is a nil brief and a warning.
func (r *runner) makeBrief(ctx context.Context, cues []srt.Cue) *Brief {
	if !r.o.Brief || len(cues) < minBriefCues {
		return nil
	}

	body, sampled := briefSource(cues)
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if sampled {
		r.warn("brief: dialogue sampled down to %d characters", briefCharBudget)
	}

	target := r.o.Target.English
	src := "the source language"
	if !r.o.Source.Zero() {
		src = r.o.Source.English
	}

	user := fmt.Sprintf(`Below is the complete dialogue of one film, one cue per line.

Analyse it and return JSON describing how it must be translated from %s into %s:
- characters: the named speakers who actually matter, at most 12, with "rendered" set to the exact spelling of the name in %s and "gender" one of male, female, unknown.
- register: at most 8 pairs of characters who address each other, the second-person form to use in %s (for Greek: "εσύ" or "εσείς"), and a reason of at most eight words. Include a pair only when the dialogue actually shows them speaking to each other.
- glossary: at most 12 recurring terms, jargon, nicknames, institutions or catchphrases, each with the single rendering to use everywhere.
- tone: one sentence on the register and style of the film as a whole.
- setting: one sentence on period, place and milieu.

Be terse. Every field is a lookup for a translator mid-sentence, not prose for an audience. Do not explain your reasoning.

DIALOGUE:
%s`, src, target, target, target, body)

	req := llm.Request{
		Stage:           "brief",
		Model:           r.o.Model,
		Temperature:     ptr(0.2),
		ReasoningEffort: reasoningEffort,
		Schema:          briefSchema,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are a script supervisor preparing a translation brief for a subtitle translator. Answer only with the JSON object."},
			{Role: llm.RoleUser, Content: user},
		},
	}

	// The guarded provider, not the raw one: CompleteJSON may spend a second
	// call repairing malformed JSON, and a call the fuse never saw is a call
	// the fuse cannot stop.
	b, _, err := llm.CompleteJSON[Brief](ctx, guarded{r}, req)
	if err != nil {
		// Degrade, never fail: the brief improves the translation, it is not a
		// precondition for it.
		r.warn("brief: unavailable, continuing without it: %v", err)
		return nil
	}
	if b.empty() {
		r.warn("brief: model returned nothing usable, continuing without it")
		return nil
	}
	return &b
}

// briefSource renders the dialogue for pass 0, one cue per line.
func briefSource(cues []srt.Cue) (text string, sampled bool) {
	lines := make([]string, 0, len(cues))
	total := 0
	for _, c := range cues {
		line := strings.TrimSpace(strings.Join(c.Lines, " "))
		if line == "" {
			continue
		}
		lines = append(lines, line)
		total += len(line) + 1
	}
	if len(lines) == 0 {
		return "", false
	}
	if total <= briefCharBudget {
		return strings.Join(lines, "\n") + "\n", false
	}

	// Over budget: take an even spread rather than a prefix. Truncating the head
	// reads only the opening of the film, which is exactly where the cast is
	// least established — a character introduced in the second half would be
	// missing from the brief, and every batch covering them would then be
	// translated without their name or their register.
	stride := (total + briefCharBudget - 1) / briefCharBudget
	var sb strings.Builder
	for i := 0; i < len(lines); i += stride {
		if sb.Len()+len(lines[i])+1 > briefCharBudget {
			break
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	return sb.String(), true
}
