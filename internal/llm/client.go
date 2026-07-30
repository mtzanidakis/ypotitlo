package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenPrice is the per-1M-token price for a model (USD), used to estimate cost
// because OpenCode Zen does not report usage.cost.
type TokenPrice struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Retry budgets, kept per failure class rather than as one number. A 429 is the
// provider asking us to slow down and is worth waiting out; a 5xx or a
// transport error is a fault we cannot influence, so we give up early instead
// of hammering a broken endpoint. The two budgets are spent independently, so a
// run that trips one still has the other.
const (
	defaultRetries429   = 5
	defaultRetriesOther = 2
)

// Backoff window. Full jitter picks uniformly from [0, min(cap, base·2^attempt)).
const (
	backoffBase      = time.Second
	backoffCap       = 32 * time.Second
	retryAfterJitter = time.Second
)

// Client is an OpenAI-compatible chat client with retry, budget, and cost
// accounting.
type Client struct {
	name          string
	baseURL       string
	apiKey        string
	keySource     string
	hc            *http.Client
	retries429    int
	retriesOther  int
	budget        *BudgetGuard
	now           func() time.Time
	sleep         func(ctx context.Context, d time.Duration) error
	reportsCost   bool
	tryJSONSchema bool // send response_format=json_schema first (false for OpenCode Zen)
	prices        map[string]TokenPrice
	extraHeaders  map[string]string

	// rand is guarded: math/rand.Rand is not safe for concurrent use and one
	// client is shared by every translate worker.
	randMu sync.Mutex
	rand   *rand.Rand
}

// Config configures a Client.
type Config struct {
	Name    string
	BaseURL string
	APIKey  string
	// KeySource names where APIKey came from ("flag -key", "env
	// YPOTITLO_API_KEY", "config.toml", "opencode auth.json") and is quoted
	// back in the 401 message. Without it "http 401" is a bug report nobody
	// can act on.
	KeySource     string
	ReportsCost   bool
	TryJSONSchema bool
	Prices        map[string]TokenPrice
	// ExtraHeaders is applied to every request. There is an unverified report
	// that Zen applies much tighter rate limits to clients it does not
	// recognize, keyed on x-opencode-client.
	//
	// TODO: measure whether setting x-opencode-client actually changes the
	// limits before sending it. Claiming to be another client on a hunch is
	// not something to do speculatively.
	ExtraHeaders map[string]string
	Budget       *BudgetGuard
	HTTP         *http.Client
	// RequestTimeout bounds one HTTP exchange. 0 means DefaultRequestTimeout.
	// Ignored when HTTP is supplied.
	RequestTimeout time.Duration

	// Retries429 and RetriesOther override the per-class retry budgets.
	Retries429   int
	RetriesOther int
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	// Rand is the jitter source, injected so backoff is deterministic in
	// tests. Nil gets a time-seeded one.
	Rand *rand.Rand
}

// DefaultRequestTimeout bounds a single HTTP exchange. Generous, because a
// reasoning model thinking about a batch of subtitles genuinely takes minutes.
const DefaultRequestTimeout = 4 * time.Minute

// newTransport builds the HTTP transport, deliberately on HTTP/1.1.
//
// The default transport negotiates HTTP/2, which multiplexes every concurrent
// worker onto a single TCP connection. Go's HTTP/2 sends no keepalive pings
// unless configured to, so a connection that dies without a FIN — a dropped NAT
// entry, a silently restarted proxy — stays ESTABLISHED and every request
// queued behind it waits. Observed exactly that: one socket with both queues
// empty, four workers idle, twenty-nine minutes of silence, no error, while the
// service answered curl normally throughout.
//
// Keeping HTTP/2 and enabling its pings would mean importing
// golang.org/x/net/http2. HTTP/2 buys this client nothing — a few dozen
// requests spread over minutes — so the multiplexing is dropped instead of
// repaired. One connection per in-flight request means a stuck request stalls
// only itself, and the client's own Timeout bounds it.
func newTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()

	// Three things are needed, and two of them alone are a trap. Clearing
	// TLSNextProto stops the transport *handling* h2, but ALPN still offers it,
	// so a server that prefers h2 selects it and then answers in a protocol
	// this transport will not parse — the symptom is
	// `malformed HTTP response "\x00\x00\x12\x04..."`, which is an HTTP/2
	// SETTINGS frame read as a status line. NextProtos has to say http/1.1 too.
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return t
}

// NewClient builds a chat client from cfg.
func NewClient(cfg Config) *Client {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	hc := cfg.HTTP
	if hc == nil {
		timeout := cfg.RequestTimeout
		if timeout == 0 {
			timeout = DefaultRequestTimeout
		}
		hc = &http.Client{Timeout: timeout, Transport: newTransport()}
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(now().UnixNano())) //nolint:gosec // jitter, not crypto
	}
	retries429 := cfg.Retries429
	if retries429 == 0 {
		retries429 = defaultRetries429
	}
	retriesOther := cfg.RetriesOther
	if retriesOther == 0 {
		retriesOther = defaultRetriesOther
	}
	return &Client{
		name: cfg.Name, baseURL: cfg.BaseURL, apiKey: cfg.APIKey, keySource: cfg.KeySource,
		hc: hc, retries429: retries429, retriesOther: retriesOther,
		budget: cfg.Budget, now: now, sleep: sleep, rand: rnd,
		reportsCost: cfg.ReportsCost, tryJSONSchema: cfg.TryJSONSchema, prices: cfg.Prices,
		extraHeaders: cfg.ExtraHeaders,
	}
}

// Name implements Provider.
func (c *Client) Name() string { return c.name }

// Complete implements Provider.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if c.budget != nil && !c.budget.Allow() {
		return Response{}, ErrBudgetExceeded
	}
	start := c.now()

	resp, err := c.do(ctx, req, c.tryJSONSchema)
	// A provider that doesn't support json_schema rejects it with 400 → retry
	// once with json_object. OpenCode Zen skips this (tryJSONSchema=false).
	if err != nil && c.tryJSONSchema && req.Schema != nil && isBadRequest(err) {
		spent := resp.Retries
		resp, err = c.do(ctx, req, false)
		resp.Retries += spent
	}

	resp.LatencyMs = c.now().Sub(start).Milliseconds()
	resp.Provider = c.name
	resp.Stage = req.Stage

	// Only a cost we actually know is charged against the guard. An unknown
	// model contributes nothing here — which is why Usage.CostKnown exists and
	// why the footer has to say "unknown" out loud rather than print $0.00.
	if err == nil && c.budget != nil && resp.Usage.CostKnown {
		c.budget.Add(resp.Usage.CostUSD)
	}
	return resp, err
}

// Models lists the models the endpoint offers, sorted by id and joined with the
// local price table (Model.Price is nil when the price is unknown).
//
// Verified live: GET /models answers 200 with no Authorization header at all,
// so `ypotitlo list-models` works before a key has been configured. The header
// is still sent whenever one is available.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	body, _, err := c.send(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Object string  `json:"object"`
		Data   []Model `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("llm: parse models: %w", err)
	}
	out := list.Data
	for i := range out {
		if p, ok := c.prices[out[i].ID]; ok {
			out[i].Price = &p
		}
	}
	slices.SortFunc(out, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (c *Client) do(ctx context.Context, req Request, useJSONSchema bool) (Response, error) {
	body := c.buildBody(req, useJSONSchema)
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	respBody, retries, err := c.send(ctx, http.MethodPost, "/chat/completions", raw)
	if err != nil {
		return Response{Retries: retries}, err
	}
	var cc chatCompletion
	if err := json.Unmarshal(respBody, &cc); err != nil {
		return Response{Retries: retries}, fmt.Errorf("llm: parse completion: %w", err)
	}
	if cc.Error != nil {
		return Response{Retries: retries}, fmt.Errorf("llm: %s", cc.Error.Message)
	}
	if len(cc.Choices) == 0 || cc.Choices[0].Message.Content == "" {
		// Distinguish a run-out-of-budget truncation (reasoning models spend
		// max_tokens on thinking and return empty content) from a genuinely
		// empty reply: the fix for one is a smaller batch, for the other a
		// different prompt.
		if len(cc.Choices) > 0 && cc.Choices[0].FinishReason == FinishLength {
			return Response{Retries: retries, FinishReason: FinishLength},
				fmt.Errorf("%w (finish_reason=length)", ErrTruncated)
		}
		return Response{Retries: retries}, ErrNoContent
	}
	resp := c.buildResponse(cc.Model, cc.Choices[0].Message.Content,
		cc.Usage.PromptTokens, cc.Usage.CompletionTokens, cc.Usage.Cost)
	resp.FinishReason = cc.Choices[0].FinishReason
	resp.Retries = retries
	return resp, nil
}

// send performs the request, retrying 429 (up to retries429) and 5xx/transport
// errors (up to retriesOther) while honoring Retry-After, and never retrying
// other 4xx. 401 maps to ErrAuth, 402/403 to ErrCreditExhausted. It reports how
// many retries it spent so the run footer can add them up.
func (c *Client) send(ctx context.Context, method, path string, raw []byte) ([]byte, int, error) {
	var (
		lastErr   error
		retries   int
		used429   int
		usedOther int
	)
retry:
	for {
		if retries > 0 {
			if err := c.sleep(ctx, c.retryDelay(lastErr, retries)); err != nil {
				return nil, retries, err
			}
		}
		httpReq, err := c.newRequest(ctx, method, path, raw)
		if err != nil {
			return nil, retries, err
		}

		resp, err := c.hc.Do(httpReq)
		if err != nil {
			lastErr = err
			if !spend(&usedOther, c.retriesOther) {
				break retry
			}
			retries++
			continue
		}
		bodyBytes, code, retryAfter := c.readResponse(resp)

		switch {
		case code >= 200 && code < 300:
			return bodyBytes, retries, nil
		case code == http.StatusUnauthorized:
			return nil, retries, c.authError(code, bodyBytes)
		case code == http.StatusPaymentRequired || code == http.StatusForbidden:
			return nil, retries, fmt.Errorf("%w: http %d: %s", ErrCreditExhausted, code, trimBody(bodyBytes))
		case code == http.StatusTooManyRequests:
			lastErr = &retryableError{code: code, retryAfter: retryAfter, body: string(bodyBytes)}
			if !spend(&used429, c.retries429) {
				break retry
			}
		case code >= 500:
			lastErr = &retryableError{code: code, retryAfter: retryAfter, body: string(bodyBytes)}
			if !spend(&usedOther, c.retriesOther) {
				break retry
			}
		default: // other 4xx → do not retry
			return nil, retries, fmt.Errorf("llm: http %d: %s", code, trimBody(bodyBytes))
		}
		retries++
	}
	if lastErr == nil {
		lastErr = errors.New("llm: request failed")
	}
	// A run that spent its 429 budget was throttled, not disconnected. The
	// distinction matters to a caller's circuit breaker: the provider is
	// answering, and asking to be asked less often.
	var re *retryableError
	if errors.As(lastErr, &re) && re.code == http.StatusTooManyRequests {
		return nil, retries, fmt.Errorf("%w after %d retries: %w", ErrRateLimited, retries, lastErr)
	}
	return nil, retries, fmt.Errorf("llm: request failed after %d retries: %w", retries, lastErr)
}

// spend consumes one unit of a retry budget, reporting whether there was one
// left to consume.
func spend(used *int, limit int) bool {
	if *used >= limit {
		return false
	}
	*used++
	return true
}

func (c *Client) newRequest(ctx context.Context, method, path string, raw []byte) (*http.Request, error) {
	var body io.Reader
	if raw != nil {
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	// Sent only when there is a key: /models answers without one, and an empty
	// "Bearer " is a header that can only ever make things worse.
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// authError explains a 401 in terms of where the key came from.
func (c *Client) authError(code int, body []byte) error {
	switch {
	case c.apiKey == "":
		return fmt.Errorf("%w: http %d: no api key configured", ErrAuth, code)
	case c.keySource == "":
		return fmt.Errorf("%w: http %d: api key rejected: %s", ErrAuth, code, trimBody(body))
	default:
		return fmt.Errorf("%w: http %d: api key from %s rejected: %s", ErrAuth, code, c.keySource, trimBody(body))
	}
}

// retryDelay is the backoff before a retry.
func (c *Client) retryDelay(lastErr error, attempt int) time.Duration {
	var re *retryableError
	if errors.As(lastErr, &re) && re.retryAfter > 0 {
		// Honor the server's deadline, plus a jittered fraction of a second:
		// every worker that hit the same 429 was handed the same Retry-After
		// and would otherwise wake in lockstep.
		return re.retryAfter + c.jitter(retryAfterJitter)
	}
	// Full jitter over an exponential window. The ported client slept exactly
	// 1s, then 2s, with no jitter at all, so N workers that hit the same 429
	// retried simultaneously — N times over.
	window := backoffCap
	if attempt < 32 {
		if w := backoffBase << attempt; w > 0 && w < window {
			window = w
		}
	}
	return c.jitter(window)
}

// jitter returns a uniform duration in [0, d), or 0 for a non-positive d.
func (c *Client) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	c.randMu.Lock()
	defer c.randMu.Unlock()
	return time.Duration(c.rand.Int63n(int64(d)))
}

func (c *Client) buildBody(req Request, useJSONSchema bool) map[string]any {
	messages := req.Messages
	body := map[string]any{"model": req.Model}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	// Sampling parameters are omitted unless set: "unset" and "0" are different
	// requests, and 0 is a value we genuinely send (the strict retry).
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if c.reportsCost {
		body["usage"] = map[string]any{"include": true}
	}
	if req.Schema != nil {
		if useJSONSchema {
			body["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name": req.Schema.Name, "strict": req.Schema.Strict, "schema": req.Schema.Schema,
				},
			}
		} else {
			body["response_format"] = map[string]any{"type": "json_object"}
			// json_object enforces valid JSON but conveys no field names, so
			// embed the schema in the prompt to pin the exact fields.
			if schemaJSON, err := json.Marshal(req.Schema.Schema); err == nil {
				messages = append(append([]Message(nil), messages...), Message{
					Role:    RoleSystem,
					Content: "Return ONLY a JSON object conforming exactly to this JSON Schema — identical field names, include every property:\n" + string(schemaJSON),
				})
			}
		}
	}
	body["messages"] = messages
	return body
}

// chatCompletion mirrors the OpenAI non-streaming response shape.
type chatCompletion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (c *Client) buildResponse(model, content string, prompt, completion int, cost float64) Response {
	known := c.reportsCost && cost != 0
	if !known {
		cost, known = c.estimateCost(model, prompt, completion)
	}
	return Response{
		Content: content, Model: model,
		Usage: Usage{
			PromptTokens: prompt, CompletionTokens: completion,
			CostUSD: cost, CostKnown: known,
		},
	}
}

// estimateCost prices a call from the table. The bool is the whole point: a
// model that is not in the table has an *unknown* cost, not a zero one, and
// collapsing the two is what lets a budget guard sit there doing nothing while
// the spend runs away.
func (c *Client) estimateCost(model string, prompt, completion int) (float64, bool) {
	p, ok := c.prices[model]
	if !ok {
		return 0, false
	}
	return float64(prompt)/1e6*p.InputPer1M + float64(completion)/1e6*p.OutputPer1M, true
}

// retryableError carries a retryable HTTP status and its Retry-After.
type retryableError struct {
	code       int
	retryAfter time.Duration
	body       string
}

func (e *retryableError) Error() string {
	return fmt.Sprintf("llm: http %d: %s", e.code, strings.TrimSpace(e.body))
}

func (c *Client) readResponse(resp *http.Response) (body []byte, code int, retryAfter time.Duration) {
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, c.parseRetryAfter(resp.Header.Get("Retry-After"))
}

// parseRetryAfter reads a Retry-After header into a duration. Both wire forms
// are accepted. The ported client only ran strconv.Atoi and returned 0 for an
// HTTP-date, silently dropping to the fixed ladder — which, against a provider
// that answers 429 with a date, meant every single rate limit.
func (c *Client) parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	// The date is measured against the injected clock, not time.Now, so a test
	// can hand the client a fixed "now" and get a deterministic delay.
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.now()); d > 0 {
			return d
		}
	}
	return 0
}

// trimBody renders a response body for an error message.
func trimBody(b []byte) string { return strings.TrimSpace(string(b)) }

func isBadRequest(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http 400")
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
