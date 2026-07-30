package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedNow is the clock every test client runs on, so HTTP-date arithmetic and
// latency are deterministic.
var fixedNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// noSleep is an injected sleep that never actually waits (tests run fast).
func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// sleeper is a noSleep that records what it was asked to wait for, so backoff
// can be asserted without any test taking a second.
type sleeper struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeper) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeper) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// openCodeOn builds a provider pointed at a test server. Sleep, clock and the
// jitter source are always injected: the suite has to be instant and repeatable.
func openCodeOn(t *testing.T, url string, opts ...func(*OpenCodeGoConfig)) *Client {
	t.Helper()
	cfg := OpenCodeGoConfig{
		APIKey:  "k",
		BaseURL: url,
		Sleep:   noSleep,
		Now:     func() time.Time { return fixedNow },
		Rand:    rand.New(rand.NewSource(1)),
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewOpenCodeGo(cfg)
}

// chatOK is a minimal successful completion body.
const chatOK = `{"model":"deepseek-v4-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func userReq(model string) Request {
	return Request{Model: model, Messages: []Message{{Role: RoleUser, Content: "x"}}}
}

func TestNonStreamingCostEstimate(t *testing.T) {
	t.Parallel()
	// No cost field → estimated from the price table (deepseek-v4-pro).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-pro","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	req := userReq("deepseek-v4-pro")
	req.Stage = "brief"
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// 1M in @1.74 + 1M out @3.48 = 5.22.
	if resp.Usage.CostUSD < 5.21 || resp.Usage.CostUSD > 5.23 {
		t.Errorf("estimated cost = %v, want ~5.22", resp.Usage.CostUSD)
	}
	if !resp.Usage.CostKnown {
		t.Error("cost of a priced model must be reported as known")
	}
	if resp.Stage != "brief" || resp.Provider != "opencode-go" {
		t.Errorf("stage/provider not carried through: %+v", resp)
	}
}

func TestUnknownModelCostIsUnknownNotZero(t *testing.T) {
	t.Parallel()
	// hy3 is a real zen/go/v1 model with no published price. Reporting its cost
	// as 0.0 would leave the budget guard permanently at zero spend, i.e.
	// switched off without ever saying so.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"hy3","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`))
	}))
	defer srv.Close()

	b := NewBudgetGuard(0.20, func() time.Time { return fixedNow }, time.UTC)
	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.Budget = b })
	resp, err := c.Complete(context.Background(), userReq("hy3"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.CostKnown {
		t.Fatal("hy3 has no published price; its cost must not be reported as known")
	}
	if resp.Usage.CostUSD != 0 {
		t.Errorf("unknown cost = %v, want 0 alongside CostKnown=false", resp.Usage.CostUSD)
	}
	// Nothing was charged, because nothing could be. That is exactly why the
	// run footer has to print "cost unknown" instead of "$0.000".
	if b.Spent() != 0 {
		t.Errorf("spent = %v, want 0 for an unpriced model", b.Spent())
	}
	var tally Tally
	tally.Add(resp)
	if got := tally.Totals().String(); !strings.Contains(got, "cost unknown") {
		t.Errorf("footer = %q, want it to say the cost is unknown", got)
	}
}

func TestUsesJSONObjectWithEmbeddedSchema(t *testing.T) {
	t.Parallel()
	var sawSchema, sawObject, sawFieldName atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages       []Message      `json:"messages"`
			ResponseFormat map[string]any `json:"response_format"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		switch body.ResponseFormat["type"] {
		case "json_schema":
			sawSchema.Store(true)
			w.WriteHeader(http.StatusBadRequest)
		case "json_object":
			sawObject.Store(true)
			for _, m := range body.Messages {
				if strings.Contains(m.Content, "verdict") {
					sawFieldName.Store(true)
				}
			}
			_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`))
		}
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	_, err := c.Complete(context.Background(), Request{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
		Schema: &JSONSchema{Name: "s", Strict: true, Schema: map[string]any{
			"type": "object", "properties": map[string]any{"verdict": map[string]any{"type": "string"}},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sawSchema.Load() {
		t.Error("opencode-go sent json_schema; it should use json_object directly")
	}
	if !sawObject.Load() {
		t.Error("opencode-go did not send json_object")
	}
	if !sawFieldName.Load() {
		t.Error("json_object request did not embed the schema field names in the prompt")
	}
}

func TestSamplingParamsSentOnlyWhenSet(t *testing.T) {
	t.Parallel()
	var got atomic.Pointer[map[string]any]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		got.Store(&body)
		_, _ = w.Write([]byte(chatOK))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)

	// Unset → the field must be absent, so the provider default applies.
	if _, err := c.Complete(context.Background(), userReq("m")); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "top_p", "seed", "reasoning_effort", "max_tokens"} {
		if _, ok := (*got.Load())[k]; ok {
			t.Errorf("unset %s must not be sent, body had it", k)
		}
	}

	// Set → sent verbatim. Zero is a value we genuinely send (the strict
	// retry), so a plain float64 field would have been unable to express it.
	zero, topP := 0.0, 0.9
	seed := 7
	req := userReq("m")
	req.Temperature, req.TopP, req.Seed = &zero, &topP, &seed
	req.ReasoningEffort, req.MaxTokens = "none", 1024
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := *got.Load()
	want := map[string]any{
		"temperature": 0.0, "top_p": 0.9, "seed": 7.0,
		"reasoning_effort": "none", "max_tokens": 1024.0,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %#v, want %#v", k, body[k], v)
		}
	}
}

func TestBudgetGuardBlocks(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// prompt/completion large enough that estimated cost exceeds the budget.
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-pro","choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`))
	}))
	defer srv.Close()

	b := NewBudgetGuard(0.20, func() time.Time { return fixedNow }, time.UTC)
	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.Budget = b })
	// First call succeeds and pushes spend (1.74) over the 0.20 limit.
	if _, err := c.Complete(context.Background(), userReq("deepseek-v4-pro")); err != nil {
		t.Fatal(err)
	}
	// Second call is blocked by the budget guard.
	_, err := c.Complete(context.Background(), userReq("deepseek-v4-pro"))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("err = %v, want ErrBudgetExceeded", err)
	}
}

func TestCreditExhaustedMapping(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusPaymentRequired, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			c := openCodeOn(t, srv.URL)
			_, err := c.Complete(context.Background(), userReq("m"))
			if !errors.Is(err, ErrCreditExhausted) {
				t.Errorf("err = %v, want ErrCreditExhausted", err)
			}
			if errors.Is(err, ErrAuth) {
				t.Error("402/403 is a billing problem, not an authentication one")
			}
		})
	}
}

func TestUnauthorizedIsErrAuthAndNamesTheKeySource(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) {
		cfg.KeySource = "env YPOTITLO_API_KEY"
	})
	_, err := c.Complete(context.Background(), userReq("m"))
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if errors.Is(err, ErrCreditExhausted) {
		t.Error("401 must not be reported as exhausted credit: the key is resolved from four places and the user needs to know which one was used")
	}
	if !strings.Contains(err.Error(), "env YPOTITLO_API_KEY") {
		t.Errorf("err = %v, want it to name the key source", err)
	}
	if calls.Load() != 1 {
		t.Errorf("401 must not be retried; got %d calls", calls.Load())
	}

	// With no key at all the message says so rather than blaming a source.
	c2 := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.APIKey = "" })
	_, err = c2.Complete(context.Background(), userReq("m"))
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "no api key configured") {
		t.Errorf("err = %v, want ErrAuth naming the missing key", err)
	}
}

func TestRetryOn503ThenSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(chatOK))
	}))
	defer srv.Close()

	var sl sleeper
	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.Sleep = sl.sleep })
	resp, err := c.Complete(context.Background(), userReq("deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls (503 + retry), got %d", calls.Load())
	}
	if resp.Retries != 1 {
		t.Errorf("resp.Retries = %d, want 1", resp.Retries)
	}
	// Retry-After: 1 honored, plus at most a second of jitter so that
	// concurrent workers handed the same deadline do not wake together.
	waits := sl.recorded()
	if len(waits) != 1 || waits[0] < time.Second || waits[0] >= 2*time.Second {
		t.Errorf("waits = %v, want one in [1s, 2s)", waits)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// The date form. The ported client ran only strconv.Atoi here, got
			// 0, and silently fell back to its fixed ladder.
			w.Header().Set("Retry-After", fixedNow.Add(30*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(chatOK))
	}))
	defer srv.Close()

	var sl sleeper
	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.Sleep = sl.sleep })
	if _, err := c.Complete(context.Background(), userReq("deepseek-v4-flash")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	waits := sl.recorded()
	if len(waits) != 1 || waits[0] < 30*time.Second || waits[0] >= 31*time.Second {
		t.Errorf("waits = %v, want one in [30s, 31s) from the HTTP-date", waits)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	c := openCodeOn(t, "http://example.invalid")
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"seconds", "12", 12 * time.Second},
		{"seconds padded", "  12  ", 12 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-5", 0},
		{"http date", fixedNow.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"http date in the past", fixedNow.Add(-time.Minute).UTC().Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := c.parseRetryAfter(tt.in); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestBackoffIsFullJitterBoundedByCap(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // no Retry-After → the ladder
	}))
	defer srv.Close()

	run := func(seed int64) []time.Duration {
		var sl sleeper
		c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) {
			cfg.Sleep = sl.sleep
			cfg.Rand = rand.New(rand.NewSource(seed))
		})
		c.retries429 = 8 // past the cap, which is reached at attempt 5
		if _, err := c.Complete(context.Background(), userReq("m")); err == nil {
			t.Fatal("expected failure after the retry budget ran out")
		}
		return sl.recorded()
	}

	waits := run(1)
	if len(waits) != 8 {
		t.Fatalf("got %d waits, want 8", len(waits))
	}
	var distinct int
	for i, d := range waits {
		attempt := i + 1
		window := backoffCap
		if w := backoffBase << attempt; w < window {
			window = w
		}
		if d < 0 || d >= window {
			t.Errorf("wait %d = %v, want [0, %v)", attempt, d, window)
		}
		if d != 0 {
			distinct++
		}
	}
	if distinct == 0 {
		t.Error("every wait was zero; the jitter source is not being used")
	}
	// Deterministic given the seed, which is the whole reason it is injected.
	if got := run(1); !equalDurations(got, waits) {
		t.Errorf("same seed gave %v then %v", waits, got)
	}
	if got := run(2); equalDurations(got, waits) {
		t.Error("different seeds produced identical backoff; the jitter is not random")
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetryBudgetIsPerStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		code      int
		wantCalls int32 // 1 initial + the budget for that class
	}{
		{"429 is worth waiting out", http.StatusTooManyRequests, 1 + defaultRetries429},
		{"5xx gives up early", http.StatusInternalServerError, 1 + defaultRetriesOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.code)
			}))
			defer srv.Close()

			c := openCodeOn(t, srv.URL)
			resp, err := c.Complete(context.Background(), userReq("m"))
			if err == nil {
				t.Fatal("expected failure")
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tt.wantCalls)
			}
			// The retries are still reported, so a failed call is counted in
			// the run footer.
			if resp.Retries != int(tt.wantCalls)-1 {
				t.Errorf("resp.Retries = %d, want %d", resp.Retries, tt.wantCalls-1)
			}
		})
	}
}

func TestNoRetryOnBadRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	_, err := c.Complete(context.Background(), userReq("m"))
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("400 must not be retried; got %d calls", calls.Load())
	}
}

func TestTruncatedAtMaxTokens(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A reasoning model that spent the whole output budget thinking.
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{}}`))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	resp, err := c.Complete(context.Background(), userReq("m"))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if errors.Is(err, ErrNoContent) {
		t.Error("a truncation must be distinguishable from an empty reply: one is fixed by splitting the batch, the other is not")
	}
	if resp.FinishReason != FinishLength {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, FinishLength)
	}
}

func TestEmptyCompletion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":""},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	if _, err := c.Complete(context.Background(), userReq("m")); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}

const modelsBody = `{"object":"list","data":[
	{"id":"minimax-m3","object":"model","created":1785368115,"owned_by":"opencode"},
	{"id":"hy3","object":"model","created":1785368115,"owned_by":"opencode"},
	{"id":"deepseek-v4-flash","object":"model","created":1785368115,"owned_by":"opencode"}
]}`

func TestModels(t *testing.T) {
	t.Parallel()
	var path, auth atomic.Value
	path.Store("")
	auth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(modelsBody))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if path.Load() != "/models" {
		t.Errorf("path = %q, want /models", path.Load())
	}
	if auth.Load() != "Bearer k" {
		t.Errorf("Authorization = %q; the key is still sent when there is one", auth.Load())
	}
	wantIDs := []string{"deepseek-v4-flash", "hy3", "minimax-m3"} // sorted by id
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d models, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("model %d = %q, want %q", i, got[i].ID, id)
		}
	}
	if got[0].Object != "model" || got[0].Created != 1785368115 || got[0].OwnedBy != "opencode" {
		t.Errorf("decoded model = %+v", got[0])
	}
	// Joined with the local price table; nil where no price is published.
	if got[0].Price == nil || got[0].Price.InputPer1M != 0.14 {
		t.Errorf("deepseek-v4-flash price = %+v, want 0.14 in", got[0].Price)
	}
	if got[1].Price != nil {
		t.Errorf("hy3 price = %+v, want nil (no published price)", got[1].Price)
	}
}

func TestModelsWithoutAPIKey(t *testing.T) {
	t.Parallel()
	// Verified live: /models answers 200 with no Authorization header, so
	// list-models has to work before a key has been configured at all.
	var hadAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Header["Authorization"]
		hadAuth.Store(ok)
		_, _ = w.Write([]byte(modelsBody))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) { cfg.APIKey = "" })
	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models without a key: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d models, want 3", len(got))
	}
	if hadAuth.Load() {
		t.Error("an empty key must not be sent as a bare \"Bearer \" header")
	}
}

func TestModelsPropagatesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL)
	if _, err := c.Models(context.Background()); err == nil {
		t.Fatal("expected an error on 404")
	}
}

func TestExtraHeadersAreSent(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	got.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("X-Opencode-Client"))
		_, _ = w.Write([]byte(chatOK))
	}))
	defer srv.Close()

	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) {
		cfg.ExtraHeaders = map[string]string{"x-opencode-client": "ypotitlo"}
	})
	if _, err := c.Complete(context.Background(), userReq("m")); err != nil {
		t.Fatal(err)
	}
	if got.Load() != "ypotitlo" {
		t.Errorf("x-opencode-client = %q, want ypotitlo", got.Load())
	}
}

func TestSleepCtx(t *testing.T) {
	t.Parallel()
	// The production sleep, exercised directly: every other test injects a fake
	// one, so this is the only place the default seam is checked.
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepCtx: %v", err)
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Errorf("a non-positive sleep on a live context is a no-op, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled instead of an hour's wait", err)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := openCodeOn(t, srv.URL, func(cfg *OpenCodeGoConfig) {
		// The real sleepCtx returns ctx.Err(); cancel during the first backoff.
		cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
	})
	if _, err := c.Complete(ctx, userReq("m")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: a cancelled run must not keep retrying", calls.Load())
	}
}
