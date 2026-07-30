package translate

import (
	"fmt"
	"strings"

	"github.com/mtzanidakis/ypotitlo/internal/llm"
)

// request renders one batch call.
//
// group is everything the model may look at; only is the subset it must emit.
// The two differ on a re-request, where the missing cues are asked for again
// with their neighbours still visible — that context is exactly what made the
// batch translatable in the first place, and dropping it to save tokens would
// make the second attempt worse than the first.
//
// note carries the reason for a repair call ("your reply omitted these ids",
// the refusal nudge) and strict switches the sampling temperature to zero:
// repair calls are about compliance, not prose.
func (r *runner) request(job batchJob, group, only []*prepared, note string, strict bool) llm.Request {
	var sb strings.Builder

	if len(job.before) > 0 {
		sb.WriteString("CONTEXT BEFORE — read for continuity. Do NOT translate these, do NOT emit them:\n")
		for _, s := range job.before {
			fmt.Fprintf(&sb, "  %s\n", s)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "CUES (%d). Each block is \"#id n=<lines>\" followed by exactly that many lines of text:\n\n", len(group))
	for _, p := range group {
		fmt.Fprintf(&sb, "#%d n=%d\n%s\n\n", p.id, p.n(), strings.Join(p.src, "\n"))
	}

	if len(job.after) > 0 {
		sb.WriteString("CONTEXT AFTER — read for continuity. Do NOT translate these, do NOT emit them:\n")
		for _, s := range job.after {
			fmt.Fprintf(&sb, "  %s\n", s)
		}
		sb.WriteString("\n")
	}

	if note != "" {
		sb.WriteString(note)
		sb.WriteString("\n\n")
	}

	fmt.Fprintf(&sb, "Emit one JSON object per line, for these cue ids exactly: %s\n", idList(only))
	sb.WriteString("Shape, one per line:\n")
	sb.WriteString(`{"i":<id>,"n":<line count>,"t":["<line 1>","<line 2>"],"s":"ok"}` + "\n")
	sb.WriteString(`If you genuinely cannot translate a cue, emit {"i":<id>,"n":<n>,"t":[],"s":"refused"} for that cue alone and translate the rest.` + "\n\n")
	sb.WriteString(recap)

	temp := tempNormal
	if strict {
		temp = tempStrict
	}

	return llm.Request{
		Stage:           "batch",
		Model:           r.o.Model,
		MaxTokens:       maxTokensFor(only),
		Temperature:     ptr(temp),
		Seed:            ptr(r.nextSeed()),
		ReasoningEffort: reasoningEffort,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: r.sys},
			{Role: llm.RoleUser, Content: sb.String()},
		},
	}
}

func idList(prep []*prepared) string {
	ids := make([]string, len(prep))
	for i, p := range prep {
		ids[i] = fmt.Sprintf("%d", p.id)
	}
	return strings.Join(ids, ", ")
}

// maxTokensFor sizes the output ceiling from the source character count.
//
// The divisor is characters-per-token for the source and the multiplier is the
// expansion budget for the target: Greek is token-expensive — an accented Greek
// word costs several tokens where its English equivalent costs one — so sizing
// the reply from the English character count without a healthy multiplier is
// how a batch gets truncated. The constant term covers the JSON envelope, which
// is a fixed ~20 tokens per cue and dominates for short cues.
func maxTokensFor(prep []*prepared) int {
	chars := 0
	for _, p := range prep {
		for _, s := range p.src {
			chars += len([]rune(s))
		}
	}
	n := int(float64(chars)/2.5*3) + 512
	if n < minBatchTokens {
		n = minBatchTokens
	}
	if n > maxBatchTokens {
		n = maxBatchTokens
	}
	return n
}

const (
	minBatchTokens = 768
	maxBatchTokens = 16384
)
