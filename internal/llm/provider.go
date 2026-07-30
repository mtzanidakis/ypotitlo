package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenCodeGoBaseURL is the OpenCode Zen open-weight endpoint. The
// Claude/GPT/Gemini models live on .../zen/v1 instead, which is why the base
// URL is configurable.
const DefaultOpenCodeGoBaseURL = "https://opencode.ai/zen/go/v1"

// DefaultOpenCodeGoPrices is the per-1M-token price table used to estimate cost
// because OpenCode Zen omits usage.cost. Overridable via config.
//
// Prices are the ones published at https://opencode.ai/docs/zen/ (read
// 2026-07-30) and cover 17 of the 23 model ids that GET /zen/go/v1/models
// returns. grok-4.5 is tiered by context length; the ≤200K tier is used here
// because a subtitle batch is a few thousand tokens and never reaches the
// other one.
//
// Deliberately absent, because the docs publish no price for them:
// mimo-v2-pro, mimo-v2-omni, mimo-v2.5-pro, mimo-v2.5, hy3, hy3-preview.
// (The table does list a "MiMo-V2.5 Free" as free, but its id is
// mimo-v2.5-free, a different model from mimo-v2.5.) Leaving them out is the
// point: estimateCost then reports their cost as *unknown*, which the footer
// prints as such. Inventing a plausible number for them would be worse than
// useless — it would be a budget guard enforcing a limit against fiction.
var DefaultOpenCodeGoPrices = map[string]TokenPrice{
	"deepseek-v4-flash": {InputPer1M: 0.14, OutputPer1M: 0.28},
	"deepseek-v4-pro":   {InputPer1M: 1.74, OutputPer1M: 3.48},
	"glm-5":             {InputPer1M: 1.00, OutputPer1M: 3.20},
	"glm-5.1":           {InputPer1M: 1.40, OutputPer1M: 4.40},
	"glm-5.2":           {InputPer1M: 1.40, OutputPer1M: 4.40},
	"grok-4.5":          {InputPer1M: 2.00, OutputPer1M: 6.00},
	"kimi-k2.5":         {InputPer1M: 0.60, OutputPer1M: 3.00},
	"kimi-k2.6":         {InputPer1M: 0.95, OutputPer1M: 4.00},
	"kimi-k2.7-code":    {InputPer1M: 0.95, OutputPer1M: 4.00},
	"kimi-k3":           {InputPer1M: 3.00, OutputPer1M: 15.00},
	"minimax-m2.5":      {InputPer1M: 0.30, OutputPer1M: 1.20},
	"minimax-m2.7":      {InputPer1M: 0.30, OutputPer1M: 1.20},
	"minimax-m3":        {InputPer1M: 0.30, OutputPer1M: 1.20},
	"qwen3.5-plus":      {InputPer1M: 0.20, OutputPer1M: 1.20},
	"qwen3.6-plus":      {InputPer1M: 0.50, OutputPer1M: 3.00},
	"qwen3.7-max":       {InputPer1M: 2.50, OutputPer1M: 7.50},
	"qwen3.7-plus":      {InputPer1M: 0.40, OutputPer1M: 1.60},
}

// OpenCodeGoConfig configures the OpenCode Zen provider.
type OpenCodeGoConfig struct {
	APIKey       string
	KeySource    string // where APIKey came from; quoted back on a 401
	Budget       *BudgetGuard
	HTTP         *http.Client
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	Rand         *rand.Rand
	Prices       map[string]TokenPrice
	ExtraHeaders map[string]string
	BaseURL      string // overridable for tests and for the zen/v1 endpoint
	OnAttempt    func() // called after every HTTP exchange; see Config.OnAttempt
}

// NewOpenCodeGo builds the OpenCode Zen provider (OpenAI-compatible; no
// usage.cost, no web plugin). It rejects `response_format: json_schema` with
// HTTP 400, so it goes straight to json_object + prompt-embedded schema +
// repair — sending json_schema first would waste a round-trip on every call.
func NewOpenCodeGo(cfg OpenCodeGoConfig) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultOpenCodeGoBaseURL
	}
	prices := cfg.Prices
	if prices == nil {
		prices = DefaultOpenCodeGoPrices
	}
	return NewClient(Config{
		Name: "opencode-go", BaseURL: base, APIKey: cfg.APIKey, KeySource: cfg.KeySource,
		ReportsCost: false, TryJSONSchema: false, Prices: prices, ExtraHeaders: cfg.ExtraHeaders,
		Budget: cfg.Budget, HTTP: cfg.HTTP, Now: cfg.Now, Sleep: cfg.Sleep, OnAttempt: cfg.OnAttempt, Rand: cfg.Rand,
	})
}

// CompleteJSON runs a structured completion and unmarshals it into T. On a JSON
// parse failure it performs a single repair round-trip, feeding the malformed
// output back with an instruction to return only valid JSON.
func CompleteJSON[T any](ctx context.Context, p Provider, req Request) (T, Response, error) {
	var out T
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return out, resp, err
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Content)), &out); err == nil {
		return out, resp, nil
	}

	// One repair attempt. The returned Response carries the combined usage of
	// both round-trips so the run footer accounts for total spend; the budget
	// guard already accounted for each call inside the client, so nothing is
	// double-counted there.
	repair := req
	repair.Messages = append(append([]Message(nil), req.Messages...),
		Message{Role: RoleAssistant, Content: resp.Content},
		Message{Role: RoleUser, Content: "Your previous reply was not valid JSON. Reply with ONLY the JSON object matching the required schema — no prose, no markdown fences."},
	)
	resp2, err := p.Complete(ctx, repair)
	resp2.Usage = combineUsage(resp.Usage, resp2.Usage)
	resp2.Retries += resp.Retries
	if err != nil {
		return out, resp2, err
	}
	if err := json.Unmarshal([]byte(extractJSON(resp2.Content)), &out); err != nil {
		return out, resp2, fmt.Errorf("llm: JSON repair failed: %w", err)
	}
	return out, resp2, nil
}

// combineUsage sums the token/cost accounting of two calls. The combined cost
// is known only if both parts were.
func combineUsage(a, b Usage) Usage {
	return Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		CostUSD:          a.CostUSD + b.CostUSD,
		CostKnown:        a.CostKnown && b.CostKnown,
	}
}

// extractJSON strips markdown code fences and surrounding prose, returning the
// substring from the first '{' to the last '}' (or the input unchanged).
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
