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

// maxTokensFor sizes the output ceiling for one call.
//
// The obvious design — size the reply from the source text, since a translation
// runs about as long as its original — wedges the run, and the reason is worth
// recording because nothing in the API documentation hints at it.
//
// On a reasoning model the thinking is billed as completion tokens and spent
// out of max_tokens *before* the first character of the answer appears.
// Measured against deepseek-v4-pro: two short sentences cost 12 content tokens
// and 199 reasoning tokens, and "hi" with a ceiling of 50 came back as
// finish_reason=length with an entirely empty message. On a real 20-cue batch
// the thinking runs to roughly ten thousand tokens. That budget is not
// published, varies per model, tracks the difficulty of the task rather than
// its length, and is not reliably reduced by reasoning_effort.
//
// Two consequences follow, and both were learned the expensive way:
//
// A ceiling derived from the source text is far too small, so every batch
// truncates. Halving the batch does not help, because the overhead is per call
// rather than per cue — a real run went 20 -> 10 -> 5 cues, truncated at every
// size, and burned the call fuse without producing a file.
//
// Omitting max_tokens entirely is the opposite mistake. It removes the
// truncation but lets a verbose reasoner spend without limit; one feature film
// then costs more than a dollar in thinking alone.
//
// So: a floor generous enough to think in, a cap to bound a runaway, and the
// source-derived term kept only to let genuinely long batches ask for more.
// Cost control proper belongs to the budget guard and the call fuse.
//
// scale raises the ceiling for a retry after a truncated reply.
func maxTokensFor(prep []*prepared, scale int) int {
	if scale < 1 {
		scale = 1
	}
	chars := 0
	for _, p := range prep {
		for _, s := range p.src {
			chars += len([]rune(s))
		}
	}
	n := (int(float64(chars)/2.5*3) + 512 + reasoningHeadroom) * scale
	if n < minBatchTokens {
		n = minBatchTokens
	}
	if n > maxBatchTokens {
		n = maxBatchTokens
	}
	return n
}

const (
	// reasoningHeadroom covers the thinking a reasoning model bills against
	// max_tokens before it answers. Measured at roughly ten thousand tokens for
	// a 20-cue batch on deepseek-v4-pro; sized above that because a ceiling
	// that is too high costs nothing unless the model actually uses it, while
	// one that is too low costs the whole batch. See maxTokensFor.
	reasoningHeadroom = 16384

	// minBatchTokens is the floor. Even a single-cue batch needs room to think,
	// and it is thinking rather than output that dominates.
	minBatchTokens = 16384

	// maxBatchTokens bounds a runaway reasoner.
	maxBatchTokens = 65536
)
