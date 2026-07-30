// Package llm is a minimal OpenAI-compatible chat client for ypotitlo. It
// targets OpenCode Zen, which rejects `response_format: json_schema` (HTTP
// 400), so structured output goes straight to `json_object` with the schema
// embedded in the prompt plus one repair round-trip (CompleteJSON). Cost is
// estimated from a price table because the provider omits usage.cost.
//
// Adapted from an earlier OpenAI-compatible client, with the fixes a port review
// called for: full jitter on backoff, 401 told apart from 402/403, Retry-After
// in HTTP-date form, per-status retry budgets, sampling parameters on Request,
// and a cost that can be *unknown* rather than silently zero.
package llm

import (
	"context"
	"errors"
)

// Errors surfaced to the CLI, which maps them to exit codes.
var (
	// ErrBudgetExceeded is returned when a call would exceed the budget.
	ErrBudgetExceeded = errors.New("llm: budget exceeded")
	// ErrAuth indicates a provider 401: the API key is missing, malformed or
	// rejected. It is deliberately distinct from ErrCreditExhausted — the key
	// comes from four different places (flag, env, config file, opencode
	// auth.json) and telling someone with a typo that their credit ran out
	// sends them looking in exactly the wrong direction.
	ErrAuth = errors.New("llm: authentication failed")
	// ErrCreditExhausted indicates a provider 402/403 (quota/credit) response.
	ErrCreditExhausted = errors.New("llm: credit/quota exhausted")
	// ErrNoContent indicates the model returned an empty completion.
	ErrNoContent = errors.New("llm: empty completion")
	// ErrTruncated indicates the model hit max_tokens (finish_reason=length).
	// The translate stage splits the batch on this instead of retrying: a
	// retry of the same size is guaranteed to be truncated again.
	ErrTruncated = errors.New("llm: truncated at max_tokens")
)

// Chat role constants.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Finish reasons we act on.
const (
	FinishLength = "length"
)

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// JSONSchema describes a structured-output schema. On OpenCode Zen it is
// embedded in the prompt (json_object mode) rather than sent as response_format.
type JSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

// Request is a single completion request.
type Request struct {
	Stage     string // "brief" | "batch" | "repair" | … — a label for the run footer
	Model     string
	Messages  []Message
	MaxTokens int

	// Sampling parameters, all optional: a nil pointer means "do not send the
	// field" and is not the same as a zero value. The ported client sent none
	// of them, so every call ran at the provider default (~1.0) — the wrong
	// end of the dial for output that has to obey a line-level format, and a
	// direct cause of retries.
	Temperature *float64
	TopP        *float64
	Seed        *int

	// ReasoningEffort ("none" | "low" | "medium" | "high", provider-dependent)
	// is sent as reasoning_effort when non-empty. Half of zen/go/v1 is
	// reasoning models, which will happily spend the whole output budget
	// thinking about a subtitle line.
	ReasoningEffort string

	Schema *JSONSchema // structured output (optional)
}

// Usage is the token/cost accounting for a call.
//
// CostKnown is false when the model is absent from the price table. The
// provider omits usage.cost, so for an unknown model there is no cost figure at
// all — and reporting that as 0.0, which the ported client did, is precisely
// what turns the budget guard into a no-op that never fires.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	CostKnown        bool
}

// Response is a completion result.
type Response struct {
	Content string
	Model   string
	Usage   Usage
	// FinishReason is passed through verbatim so the caller can react to a
	// truncation that still carried partial content (ErrTruncated is only
	// returned when the truncated reply was *also* empty).
	FinishReason string
	Provider     string
	Stage        string
	Retries      int // retries spent on this call, for the run footer
	LatencyMs    int64
}

// Model is one entry of GET /models, joined with the local price table.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Price is nil when the model is not in the price table, i.e. when its cost
	// cannot be estimated at all. Callers must render that as "unknown".
	Price *TokenPrice `json:"-"`
}

// Provider is the interface the translate stage depends on.
type Provider interface {
	// Complete performs a completion. When req.Schema is set the returned
	// Content is JSON the caller unmarshals (see CompleteJSON for repair).
	Complete(ctx context.Context, req Request) (Response, error)
	// Name identifies the provider ("opencode-go").
	Name() string
}
