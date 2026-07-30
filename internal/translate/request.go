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
//
// tokenScale multiplies the output ceiling: 1 for an ordinary call, larger when
// retrying a reply that was cut off.
func (r *runner) request(job batchJob, group, only []*prepared, note string, strict bool, tokenScale int) llm.Request {
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
		MaxTokens:       maxTokensFor(only, tokenScale),
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

// maxTokensFor is the output ceiling for one call: a flat value, escalated once
// when a reply comes back cut off.
//
// It used to be derived from the source text, on the theory that a translation
// runs about as long as its original. That was wrong three times over, and the
// arithmetic was in the end dead code — the floor always won. The number is not
// about the text at all: a reasoning model bills its thinking as completion
// tokens and spends it before emitting anything, and that budget tracks the
// difficulty of the task rather than its length. Measured against
// deepseek-v4-flash on a 734-cue episode, a ceiling of roughly 17k truncated
// nearly every 20-cue batch, so every batch cost two calls.
//
// Sizing it generously costs nothing that matters: unused headroom is not
// billed, only tokens actually produced are, and the real spending limits are
// the budget guard and the call fuse. A ceiling that is too small, by contrast,
// costs a doubled call count and then a split cascade.
func maxTokensFor(_ []*prepared, scale int) int {
	if scale > 1 {
		return escalatedBatchTokens
	}
	return batchTokens
}

const (
	// batchTokens is what an ordinary call asks for.
	batchTokens = 65536

	// escalatedBatchTokens is the retry after a truncated reply. It is what
	// rescued those truncated batches in practice.
	escalatedBatchTokens = 131072
)
