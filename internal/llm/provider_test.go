package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompleteJSONRepairPath(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"content":"Sure! {bad json"}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"content":"{\"tone\":\"wry\",\"cues\":12}"}}],"usage":{"prompt_tokens":120,"completion_tokens":15}}`))
	}))
	defer srv.Close()

	type brief struct {
		Tone string `json:"tone"`
		Cues int    `json:"cues"`
	}
	c := openCodeOn(t, srv.URL)
	out, resp, err := CompleteJSON[brief](context.Background(), c, Request{
		Model: "deepseek-v4-flash", Messages: []Message{{Role: RoleUser, Content: "summarize"}},
		Schema: &JSONSchema{Name: "s", Strict: true, Schema: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatalf("CompleteJSON: %v", err)
	}
	if out.Tone != "wry" || out.Cues != 12 {
		t.Errorf("out = %+v", out)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls (initial + repair), got %d", calls.Load())
	}
	// The returned usage covers BOTH round-trips so the run footer accounts for
	// total spend (100+120 in, 10+15 out).
	if resp.Usage.PromptTokens != 220 || resp.Usage.CompletionTokens != 25 {
		t.Errorf("combined usage = %+v, want prompt 220 / completion 25", resp.Usage)
	}
	// deepseek-v4-flash: 220/1e6*0.14 + 25/1e6*0.28.
	wantCost := 220.0/1e6*0.14 + 25.0/1e6*0.28
	if diff := resp.Usage.CostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("combined cost = %v, want %v", resp.Usage.CostUSD, wantCost)
	}
	if !resp.Usage.CostKnown {
		t.Error("both round-trips were priced, so the combined cost is known")
	}
}

func TestCompleteJSONGivesUpAfterOneRepair(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"nope"}}],"usage":{}}`))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	_, _, err := CompleteJSON[map[string]any](context.Background(), c, Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
		Schema: &JSONSchema{Name: "s", Schema: map[string]any{"type": "object"}},
	})
	if err == nil || !strings.Contains(err.Error(), "JSON repair failed") {
		t.Fatalf("err = %v, want a repair failure", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want exactly 2 (one repair, not a loop)", calls.Load())
	}
}

func TestCompleteJSONPropagatesCompleteError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.KeySource = "config.toml" })
	_, _, err := CompleteJSON[map[string]any](context.Background(), c, Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

func TestExtractJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced anonymous", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"preamble", "Sure!\n{\"a\":1}", `{"a":1}`},
		{"trailing chatter", "{\"a\":1}\nHope that helps!", `{"a":1}`},
		{"nested", `x {"a":{"b":2}} y`, `{"a":{"b":2}}`},
		{"no object", "no json here", "no json here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := extractJSON(tt.in); got != tt.want {
				t.Errorf("extractJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultPricesCoverOnlyPublishedModels(t *testing.T) {
	t.Parallel()
	// The 23 ids GET /zen/go/v1/models returned when this was written. The
	// six with no published price are asserted to be *absent*, so that a
	// well-meaning future edit cannot quietly invent a number for them: an
	// invented price is a budget guard enforcing a limit against fiction.
	priced := []string{
		"deepseek-v4-flash", "deepseek-v4-pro",
		"glm-5", "glm-5.1", "glm-5.2", "grok-4.5",
		"kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code", "kimi-k3",
		"minimax-m2.5", "minimax-m2.7", "minimax-m3",
		"qwen3.5-plus", "qwen3.6-plus", "qwen3.7-max", "qwen3.7-plus",
	}
	unpriced := []string{
		"mimo-v2-pro", "mimo-v2-omni", "mimo-v2.5-pro", "mimo-v2.5", "hy3", "hy3-preview",
	}
	for _, id := range priced {
		p, ok := DefaultOpenCodeGoPrices[id]
		if !ok {
			t.Errorf("%s has a published price and is missing from the table", id)
			continue
		}
		if p.InputPer1M <= 0 || p.OutputPer1M <= 0 {
			t.Errorf("%s priced at %+v", id, p)
		}
	}
	for _, id := range unpriced {
		if p, ok := DefaultOpenCodeGoPrices[id]; ok {
			t.Errorf("%s has no published price but the table claims %+v", id, p)
		}
	}
	if len(DefaultOpenCodeGoPrices) != len(priced) {
		t.Errorf("table has %d entries, want exactly the %d published ones", len(DefaultOpenCodeGoPrices), len(priced))
	}
}

func TestNewOpenCodeGoDefaults(t *testing.T) {
	t.Parallel()
	c := NewOpenCodeGo(OpenCodeGoConfig{APIKey: "k"})
	if c.Name() != "opencode-go" {
		t.Errorf("name = %q", c.Name())
	}
	if c.baseURL != DefaultOpenCodeGoBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultOpenCodeGoBaseURL)
	}
	if c.tryJSONSchema {
		t.Error("opencode zen rejects json_schema with a 400; it must not be tried")
	}
	if c.retries429 != defaultRetries429 || c.retriesOther != defaultRetriesOther {
		t.Errorf("retry budgets = %d/%d, want %d/%d",
			c.retries429, c.retriesOther, defaultRetries429, defaultRetriesOther)
	}
	if c.rand == nil {
		t.Error("a nil Config.Rand must be replaced by a seeded source, not left nil")
	}
}
