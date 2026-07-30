// Package translate turns a parsed subtitle file into another language.
//
// The contract with the caller is narrow and absolute: the result has exactly
// as many cues as the input, in the same order, with Index, Start and End
// untouched. Only Lines ever change, and a cue that could not be translated
// keeps its original lines, is counted in Stats.Untranslated and produces a
// warning. There is no path through this package that drops, merges, reorders
// or re-times a cue — a subtitle file that has been silently de-synchronised is
// worse than no translation at all, because nothing about it looks wrong until
// it is played.
//
// The pipeline is:
//
//	pass 0   one call over the whole file for a translation brief (brief.go)
//	batching scene-aware grouping with ±3 source cues of context (batch.go)
//	batches  JSON Lines request/response, run concurrently (protocol.go)
//	repair   four failure causes, four distinct responses (see runBatch)
//
// Every failure mode ends in "fall back and warn" rather than an error, so
// Options.Warn is the seam the tests are written against. Nothing in this
// package writes to os.Stderr.
package translate

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

const (
	defaultBatchSize   = 20
	defaultConcurrency = 2

	// maxSplitDepth bounds the recursion when a batch has to be halved. Two
	// levels turn a 20-cue batch into 5-cue batches, which is small enough that
	// a third level would be diagnosing a broken model rather than a long reply.
	maxSplitDepth = 2

	// watchdogPoll is how often the stall watchdog looks at the clock.
	watchdogPoll = 2 * time.Second

	// briefTimeout bounds pass 0, and is sized from a distribution rather than a
	// single sample. Over 26 runs against deepseek-v4-flash the call's p90 was
	// 156 s and its worst 162 s, so two calls at p90 is 312 s — which the earlier
	// 240 s did not fit at all, and is why it kept firing. CompleteJSON makes
	// that second call when the JSON needs repairing; empirically it parsed first
	// try in all 26 runs, so the room for it is insurance rather than the common
	// path.
	//
	// It has to stay under defaultStallTimeout: the watchdog's clock starts
	// before pass 0, so a brief outlasting the stall budget kills the whole run.
	briefTimeout = 5*time.Minute + 30*time.Second

	// defaultStallTimeout is how long a run may go without a single batch
	// completing. Comfortably above the slowest observed batch on a reasoning
	// model, and far below the time a hung connection would otherwise burn.
	defaultStallTimeout = 6 * time.Minute

	// maxConsecutiveFailures aborts a run whose calls have all stopped working.
	// Set above the retries a single flaky batch can legitimately produce, and
	// far below the number of batches in a film, so a real outage is caught in
	// seconds rather than after every batch has failed in turn.
	maxConsecutiveFailures = 5

	// defaultMaxUntranslated is the share of cues that may keep their original
	// text before the run is called a failure rather than a translation.
	defaultMaxUntranslated = 0.5

	// minCuesForRatioCheck is the size below which that share proves nothing:
	// one honest fallback in a three-cue file is already a third of it.
	minCuesForRatioCheck = 10

	// truncationScale is how much the output ceiling is raised when a reply
	// comes back cut off. It is deliberately a large step rather than a
	// doubling: the shortfall is usually a reasoning model's thinking budget,
	// which is a fixed cost per call, so an incremental increase would just buy
	// another truncated reply at a higher price.
	truncationScale = 4

	// reasoningEffort is set low because half of the models this tool can reach
	// are reasoning models, and a reasoning model asked to translate twenty
	// subtitle lines will happily spend its entire output budget deliberating
	// and return nothing.
	reasoningEffort = "low"

	// tempNormal is low enough for format compliance and high enough that the
	// model still writes idiomatic dialogue. tempStrict is used for repair
	// calls, where only compliance matters.
	tempNormal = 0.25
	tempStrict = 0.0
)

// ErrCallBudget is returned when a run would exceed Options.MaxCalls.
//
// It is a fuse, not a retry policy: a run that has already spent three times
// its expected number of calls is not converging, and the only thing more calls
// buy is a larger bill.
var ErrCallBudget = errors.New("translate: call budget exhausted")

// ErrStalled is returned when a run stops making progress, typically because a
// request is hanging rather than failing.
var ErrStalled = errors.New("translate: the run stopped making progress")

// ErrProviderUnreachable is returned when calls stop working altogether.
var ErrProviderUnreachable = errors.New("translate: the provider is not answering")

// ErrMostlyUntranslated is returned when so few cues came back that the result
// is not a translation.
var ErrMostlyUntranslated = errors.New("translate: too much of the file came back untranslated")

// Options configures a run.
type Options struct {
	Provider llm.Provider
	Model    string

	// Source is the language of the input. The zero value means unknown, which
	// is a supported and common case: modern models detect it perfectly well
	// from the text, and naming it wrongly is worse than not naming it.
	Source lang.Lang
	Target lang.Lang

	BatchSize   int // cues per batch; 0 means 20
	Concurrency int // concurrent batches; 0 means 2

	// MaxCalls is the hard fuse on provider calls for the whole run. 0 means
	// 3*ceil(cues/BatchSize)+10, which covers a brief, one repair per batch and
	// a couple of splits.
	MaxCalls int

	// StallTimeout aborts the run when nothing has advanced within it.
	// 0 means defaultStallTimeout.
	StallTimeout time.Duration

	// BriefTimeout bounds pass 0. 0 means briefTimeout. It is separate from
	// StallTimeout because the brief is optional: exceeding this is a warning
	// and the translation carries on, whereas exceeding StallTimeout is fatal.
	BriefTimeout time.Duration

	// Now is the clock the stall watchdog reads, injected for tests.
	Now func() time.Time

	// MaxUntranslatedRatio is the share of cues that may keep their original
	// text before Run reports failure instead of a result. 0 means 0.5.
	MaxUntranslatedRatio float64

	// Brief enables pass 0 (see brief.go). Note that the zero value is false:
	// callers that want the brief must ask for it. The command line asks for it
	// by default.
	Brief bool

	// Rand seeds the per-call sampling seed, injected so that a test sees a
	// reproducible request stream.
	Rand *rand.Rand

	// Warn receives every recoverable problem. It is the only output channel
	// this package has; it never writes to a stream of its own. It may be
	// called from several goroutines, but calls are serialised.
	Warn func(format string, a ...any)

	// Progress is called as batches complete, with the number of cues finished
	// and the total. Serialised like Warn.
	Progress func(done, total int)

	// Debug receives detail that is only useful while diagnosing a failure —
	// notably the text of a reply that could not be parsed. It is separate from
	// Warn because a warning says what happened and this says what it looked
	// like, which is noise on an ordinary run and the whole story on a bad one.
	// Nil disables it. Serialised like Warn.
	Debug func(format string, a ...any)

	// Phase is called when the run moves to a new stage ("brief",
	// "translating"), so a caller drawing a status line can say what is
	// happening rather than leaving minutes of silence. Serialised like Warn.
	Phase func(name string)
}

// Stats is the accounting for one run.
type Stats struct {
	Calls           int // provider calls made, including the brief and repairs
	Retries         int // repair calls: strict retries, re-requests, refusal retries
	ProviderRetries int // HTTP-level retries the llm client reported
	Splits          int // batches halved after a truncated reply
	Batches         int
	Untranslated    int // cues returned in their original language
	Refusals        int // cues the model declined at least once

	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	UnknownCost      int // calls whose model has no published price
}

// Result is the outcome of a run.
type Result struct {
	// Cues has the same length and order as the input, with Start, End and
	// Index untouched. On error it is nil.
	Cues     []srt.Cue
	Brief    *Brief
	Stats    Stats
	Warnings []string
}

// Run translates cues into o.Target.
//
// It returns an error only for conditions no fallback can survive: a missing
// provider or target, an exhausted call budget, a rejected API key, an
// exhausted account, or a cancelled context. Everything else — a truncated
// reply, a malformed line, a refused cue, mangled markup, a failed brief — is
// handled, warned about, and reflected in Stats.
func Run(ctx context.Context, cues []srt.Cue, o Options) (Result, error) {
	r := newRunner(o)

	// An empty file is not an error and must not cost a single call.
	if len(cues) == 0 {
		return Result{Cues: []srt.Cue{}}, nil
	}
	if o.Provider == nil {
		return Result{}, errors.New("translate: no provider configured")
	}
	if o.Target.Zero() {
		return Result{}, errors.New("translate: no target language")
	}
	if o.Model == "" {
		return Result{}, errors.New("translate: no model configured")
	}

	r.out = make([][]string, len(cues))
	r.total = len(cues)
	r.budget(len(cues))

	// The watchdog covers the whole run, not just the batch phase. Pass 0 is a
	// provider call like any other and can hang like any other: an earlier
	// version armed the watchdog inside work(), and a brief that took twelve
	// minutes to fail went entirely unwatched.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancel = cancel
	r.lastProgress = r.clock()
	stop := r.watchdog(ctx)
	defer stop()

	// Pass 0 runs under its own, much shorter deadline. It is optional by
	// design — every failure path warns and carries on without it — so it must
	// not be able to consume the whole run. Before this, a brief that hung ate
	// the entire stall budget and the watchdog failed the run, which is the
	// opposite of "continuing without it": an otherwise healthy translation died
	// in a phase it did not need.
	r.phase("brief")
	briefCtx, cancelBrief := context.WithTimeout(ctx, r.briefDeadline())
	brief := r.makeBrief(briefCtx, cues)
	cancelBrief()

	// Pass 0 finishing is progress, even when it failed: the call came back.
	// Its failure is also not evidence about the provider — the brief is bounded
	// by its own deadline, so a timeout there says nothing about the translation
	// that follows. Leaving it in the counter started the run one failure closer
	// to tripping the breaker.
	r.markProgress()
	r.resetFailures()

	// Only a cancellation of the *parent* is fatal here — the watchdog tripping,
	// or the user interrupting. The brief's own deadline is not.
	if err := r.aborted(); err != nil {
		return Result{Stats: r.snapshot(), Warnings: r.warningList()}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{Stats: r.snapshot(), Warnings: r.warningList()}, err
	}
	r.sys = systemPrompt(o.Source, o.Target, brief)

	ranges := planBatches(cues, r.o.BatchSize)
	r.stats.Batches = len(ranges)
	r.phase("translating")

	if err := r.work(ctx, cues, ranges); err != nil {
		return Result{Stats: r.snapshot(), Warnings: r.warningList(), Brief: brief}, err
	}

	res := Result{
		Cues:     r.assemble(cues),
		Brief:    brief,
		Stats:    r.snapshot(),
		Warnings: r.warningList(),
	}

	// Keeping a handful of cues in the source language is the documented
	// fallback. Keeping most of them is not a translation, and returning one as
	// a success is how a broken run ends up written to disk and shipped: every
	// individual failure was warned about, so nothing looked wrong.
	//
	// The floor matters. On a three-cue file one legitimate fallback is already
	// a third of the file, which says nothing about whether the run worked; the
	// ratio only becomes evidence once there are enough cues to average over.
	limit := r.o.MaxUntranslatedRatio
	if limit <= 0 {
		limit = defaultMaxUntranslated
	}
	if len(cues) >= minCuesForRatioCheck {
		if ratio := float64(res.Stats.Untranslated) / float64(len(cues)); ratio > limit {
			return res, fmt.Errorf("%w: %d of %d cues (%.0f%%)",
				ErrMostlyUntranslated, res.Stats.Untranslated, len(cues), ratio*100)
		}
	}
	return res, nil
}

// runner holds the mutable state of one run.
type runner struct {
	o        Options
	sys      string
	maxCalls int
	total    int

	// out[i] is the translated text of cue i, or nil to keep the original.
	// Batches own disjoint index ranges, so workers never touch the same slot.
	out [][]string

	mu       sync.Mutex
	stats    Stats
	warnings []string
	done     int

	// consecutiveFailures drives the circuit breaker; see trip.
	consecutiveFailures int
	lastFailure         error

	// lastProgress is when the run last advanced; see watchdog.
	lastProgress time.Time

	// cbMu serialises the caller's callbacks without holding mu; see the comment
	// above phase.
	cbMu sync.Mutex

	// abort state, shared by the watchdog and the workers so that whichever
	// notices trouble first stops the whole run.
	cancel    context.CancelFunc
	abortOnce sync.Once
	abortErr  error
}

// abort records the first fatal condition and cancels the run.
func (r *runner) abort(err error) {
	r.abortOnce.Do(func() {
		r.mu.Lock()
		r.abortErr = err
		r.mu.Unlock()
		if r.cancel != nil {
			r.cancel()
		}
	})
}

// resetFailures clears the circuit breaker's counter.
func (r *runner) resetFailures() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecutiveFailures = 0
}

func (r *runner) aborted() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortErr
}

// nextSeed draws the sampling seed for one call.
//
// Every call gets its own, from the injected source, for two reasons. Tests
// need the request stream to be reproducible. And a strict retry runs at
// temperature 0, so re-using the previous call's seed would ask the model to
// reproduce, as exactly as it can, the malformed reply that caused the retry.
func (r *runner) nextSeed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.o.Rand.Intn(1 << 30)
}

func newRunner(o Options) *runner {
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.Rand == nil {
		o.Rand = rand.New(rand.NewSource(1))
	}
	return &runner{o: o}
}

// budget sets the call fuse for a run of n cues.
func (r *runner) budget(n int) {
	if r.o.MaxCalls > 0 {
		r.maxCalls = r.o.MaxCalls
		return
	}
	batches := (n + r.o.BatchSize - 1) / r.o.BatchSize
	r.maxCalls = 3*batches + 10
}

// debug reports diagnostic detail, if the caller asked for it.
func (r *runner) debug(format string, a ...any) {
	if r.o.Debug == nil {
		return
	}
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.o.Debug(format, a...)
}

// phase reports a stage change to the caller, if it asked for them.
// The callbacks below are serialised on their own mutex rather than on r.mu.
//
// Holding the run's central lock across a caller's callback means terminal I/O
// runs under it, and a blocked write to stderr — a stopped pager, flow control —
// would then freeze every worker *and* idleFor, so the stall watchdog could not
// fire. That is precisely the class of undetectable hang the watchdog exists to
// catch, so it must not be reachable through the reporting path.
func (r *runner) phase(name string) {
	if r.o.Phase == nil {
		return
	}
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.o.Phase(name)
}

func (r *runner) warn(format string, a ...any) {
	r.mu.Lock()
	r.warnings = append(r.warnings, fmt.Sprintf(format, a...))
	r.mu.Unlock()

	if r.o.Warn == nil {
		return
	}
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.o.Warn(format, a...)
}

// reserve claims one call against the fuse.
func (r *runner) reserve() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxCalls > 0 && r.stats.Calls >= r.maxCalls {
		return fmt.Errorf("%w: %d calls", ErrCallBudget, r.stats.Calls)
	}
	r.stats.Calls++
	return nil
}

// record folds one response into the run accounting. It is called for failed
// calls too: a call that errored after three HTTP retries still cost money.
func (r *runner) record(resp llm.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.ProviderRetries += resp.Retries
	r.stats.PromptTokens += resp.Usage.PromptTokens
	r.stats.CompletionTokens += resp.Usage.CompletionTokens
	if resp.Usage.CostKnown {
		r.stats.CostUSD += resp.Usage.CostUSD
	} else if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		r.stats.UnknownCost++
	}
}

func (r *runner) bump(f func(s *Stats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.stats)
}

func (r *runner) snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *runner) warningList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.warnings)
}

func (r *runner) progress(n int) {
	r.mu.Lock()
	r.done += n
	done, total := r.done, r.total
	r.mu.Unlock()

	if r.o.Progress == nil {
		return
	}
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.o.Progress(done, total)
}

// call performs one provider call under the fuse.
func (r *runner) call(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := r.reserve(); err != nil {
		return llm.Response{}, err
	}
	resp, err := r.o.Provider.Complete(ctx, req)
	r.record(resp)

	// A call that came back at all is proof the provider is answering, which is
	// the only thing the watchdog exists to detect. Resetting on batch
	// completion instead was too coarse: one batch is up to four sequential
	// calls, and one call is up to six HTTP attempts with backoff — a provider
	// answering 429 with Retry-After: 60 five times spends five minutes inside a
	// single call that ultimately succeeds, and the watchdog would kill the run
	// and blame a silent connection.
	r.markProgress()

	if err := r.trip(err); err != nil {
		return resp, err
	}
	return resp, err
}

// trip is the circuit breaker: it aborts the run once calls stop working
// altogether, instead of letting every batch fail politely.
//
// Without it, a provider that cannot be reached at all is indistinguishable
// from a successful run. Each batch fails, each failure is warned about, each
// cue quietly keeps its original text, and the command exits 0 having written a
// file in the source language. That happened during a DNS outage: the run spent
// twenty-five minutes failing and would have reported success.
//
// A truncated reply is deliberately not counted. It is a handled condition with
// its own remedy — raise the ceiling, then split — and the call fuse already
// bounds the case where that never converges.
func (r *runner) trip(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err == nil || errors.Is(err, llm.ErrTruncated) || errors.Is(err, llm.ErrRateLimited) {
		r.consecutiveFailures = 0
		return nil
	}

	r.consecutiveFailures++
	r.lastFailure = err

	// The counter is shared by every worker, so the threshold has to scale with
	// them: four workers meeting one bad minute each produce four failures with
	// no success in between, which says nothing about the provider being dead.
	limit := maxConsecutiveFailures * max(1, r.o.Concurrency)
	if r.consecutiveFailures < limit {
		return nil
	}
	return fmt.Errorf("%w after %d consecutive failures: %w",
		ErrProviderUnreachable, r.consecutiveFailures, r.lastFailure)
}

// guarded is the runner's provider seen as an llm.Provider, so that helpers
// which may make more than one call — llm.CompleteJSON does exactly that when
// it repairs malformed JSON — are still counted and still fused.
type guarded struct{ r *runner }

func (g guarded) Name() string { return g.r.o.Provider.Name() }

func (g guarded) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	return g.r.call(ctx, req)
}

// work runs the batches over o.Concurrency goroutines.
// watchdog aborts the run when nothing has completed for StallTimeout.
//
// It watches progress rather than errors, which is what makes it catch the case
// the circuit breaker cannot: a request that hangs returns neither a result nor
// a failure, so a counter of failures stays at zero forever while the run sits
// there. Any completed batch resets the clock, so a slow model is not mistaken
// for a stuck one.
func (r *runner) watchdog(ctx context.Context) (stop func()) {
	limit := r.effectiveStallTimeout()

	// The poll interval is short and independent of the limit. Waking a few
	// times a minute costs nothing, and tying it to the limit would make the
	// watchdog's own responsiveness depend on the value being watched — which
	// also makes it untestable without waiting out a real timeout.
	interval := min(limit/4, watchdogPoll)

	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if idle := r.idleFor(); idle >= limit {
					r.abort(fmt.Errorf("%w: nothing advanced for %s", ErrStalled, idle.Round(time.Second)))
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// briefDeadline is how long pass 0 may take.
func (r *runner) briefDeadline() time.Duration {
	if r.o.BriefTimeout > 0 {
		return r.o.BriefTimeout
	}
	return briefTimeout
}

// effectiveStallTimeout is the stall budget actually applied.
//
// StallTimeout is exported and unclamped, and a value at or below briefTimeout
// would turn a brief that is merely slow into a failed run — the exact
// regression the brief's own deadline was added to remove.
func (r *runner) effectiveStallTimeout() time.Duration {
	limit := r.o.StallTimeout
	if limit <= 0 {
		limit = defaultStallTimeout
	}
	if brief := r.briefDeadline(); limit <= brief {
		limit = brief + defaultStallTimeout/2
	}
	return limit
}

// idleFor is how long it has been since the run last advanced.
func (r *runner) idleFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock().Sub(r.lastProgress)
}

// markProgress restarts the watchdog's clock.
func (r *runner) markProgress() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastProgress = r.clock()
}

func (r *runner) clock() time.Time {
	if r.o.Now != nil {
		return r.o.Now()
	}
	return time.Now()
}

func (r *runner) work(ctx context.Context, cues []srt.Cue, ranges []batchRange) error {
	jobs := make(chan int)
	workers := min(r.o.Concurrency, len(ranges))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := r.runBatch(ctx, cues, ranges[i], i+1); err != nil {
					r.abort(err)
					return
				}
				r.markProgress()
				r.progress(ranges[i].end - ranges[i].start)
			}
		}()
	}

	for i := range ranges {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()

	if err := r.aborted(); err != nil {
		return err
	}
	// A cancellation that no worker reported still has to be an error: the
	// caller must never mistake a half-translated file for a finished one.
	return ctx.Err()
}

// assemble builds the output cues. Timings and indices come from the input
// unchanged; Lines are cloned so that the result never aliases the input.
func (r *runner) assemble(cues []srt.Cue) []srt.Cue {
	out := make([]srt.Cue, len(cues))
	for i, c := range cues {
		out[i] = c
		if r.out[i] != nil {
			out[i].Lines = r.out[i]
		} else {
			out[i].Lines = slices.Clone(c.Lines)
		}
	}
	return out
}

// prepared is one cue on its way to the model.
type prepared struct {
	idx   int         // index into the input cues
	id    int         // batch-local id, 1..N
	parts []lineParts // one per source line, in order
	core  []int       // indices into parts of the lines that carry text
	src   []string    // the text actually sent, one per core line
}

func (p *prepared) n() int { return len(p.src) }

// prepare decomposes a range of cues. Cues with no translatable text — empty
// cues, whitespace-only cues, cues that are nothing but an ASS override — are
// left out of the batch entirely rather than being sent as empty strings and
// counted as failures when nothing comes back.
func (r *runner) prepare(cues []srt.Cue, br batchRange) []*prepared {
	var out []*prepared
	for i := br.start; i < br.end; i++ {
		p := &prepared{idx: i}
		for _, line := range cues[i].Lines {
			lp := splitLine(line)
			if len(lp.mid) > 0 {
				r.warn("cue %d: %d ASS override block(s) inside the text will be re-attached at the end of the line",
					i+1, len(lp.mid))
			}
			if lp.core != "" {
				p.core = append(p.core, len(p.parts))
				p.src = append(p.src, lp.core)
			}
			p.parts = append(p.parts, lp)
		}
		if p.n() == 0 {
			continue
		}
		p.id = len(out) + 1
		out = append(out, p)
	}
	return out
}

// runBatch translates one batch. It returns an error only for fatal conditions.
func (r *runner) runBatch(ctx context.Context, cues []srt.Cue, br batchRange, id int) error {
	prep := r.prepare(cues, br)
	if len(prep) == 0 {
		return nil
	}
	job := batchJob{
		id:     id,
		before: contextText(cues, max(0, br.start-contextCues), br.start),
		after:  contextText(cues, br.end, min(len(cues), br.end+contextCues)),
	}
	return r.translateGroup(ctx, job, prep, 0, 1)
}

// batchJob is the read-only context around a group of cues.
type batchJob struct {
	id     int
	before []string
	after  []string
}

func contextText(cues []srt.Cue, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		if s := strings.TrimSpace(strings.Join(cues[i].Lines, " ")); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// translateGroup is the failure-classification machine.
//
// Four causes get four responses, and the distinction is the whole point. A
// single "retry once, then split in half" rule costs 4N-2 calls in the worst
// case — thousands of calls for one feature film — and applies the most
// expensive remedy to the cheapest problem.
//
//	truncated reply    split immediately, no retry. Re-asking for the same
//	                   number of cues with the same token ceiling is certain to
//	                   truncate again, so the retry is pure waste.
//	nothing parseable  one strict retry at temperature 0 naming the violation.
//	partial reply      re-request only the missing ids, with the whole batch
//	                   still visible as context. With JSON Lines this is the
//	                   ordinary path, and it is strictly better than halving:
//	                   fewer calls, and the cues that did arrive are not
//	                   translated a second time.
//	refusal            one retry for the refused cues only, saying what the
//	                   material is. Then keep the original and warn.
func (r *runner) translateGroup(ctx context.Context, job batchJob, prep []*prepared, depth, scale int) error {
	got := make(map[int]entry, len(prep))
	skipped := 0

	resp, err := r.call(ctx, r.request(job, prep, prep, "", false, scale))

	// A cut-off reply gets a higher ceiling before it gets a smaller batch.
	//
	// Halving is the right remedy only when the *content* did not fit. On a
	// reasoning model the thinking is billed as completion tokens and is spent
	// before any reply is emitted, and its size tracks the task rather than the
	// number of cues — so halving leaves the overhead untouched. Splitting
	// first produced exactly that: a batch going 20 -> 10 -> 5 cues and
	// truncating at every size, burning calls to reach the same place.
	if !isFatal(err) && truncated(resp, err) {
		r.warn("batch %d: the reply was cut off at the token ceiling; retrying with a higher one", job.id)
		r.bump(func(s *Stats) { s.Retries++ })
		resp, err = r.call(ctx, r.request(job, prep, prep, "", false, truncationScale))
	}

	switch {
	case isFatal(err):
		return err
	case truncated(resp, err):
		return r.splitGroup(ctx, job, prep, depth, truncationScale, "the reply hit the output-token limit even at a raised ceiling")
	case err != nil:
		r.warn("batch %d: call failed: %v", job.id, err)
	default:
		parsed, n := parseJSONL(resp.Content)
		skipped = n
		merge(got, parsed, prep, false)
	}

	// Cause 2 and cause 4: nothing usable came back at all.
	if len(got) == 0 {
		note := "your previous reply contained no JSON object lines at all"
		if skipped > 0 {
			note = fmt.Sprintf("your previous reply had %d line(s) that were not a JSON object matching the required shape, and no usable ones", skipped)
		}
		if err == nil && looksRefusal(resp.Content) {
			note = refusalNudge
			r.bump(func(s *Stats) { s.Refusals += len(prep) })
			r.warn("batch %d: model refused the whole batch; retrying once", job.id)
			r.debug("batch %d: refusal read as: %s", job.id, snippet(resp.Content))
		} else {
			r.warn("batch %d: unparseable reply; retrying once at temperature 0", job.id)
			r.debug("batch %d: reply began: %s", job.id, snippet(resp.Content))
		}
		r.bump(func(s *Stats) { s.Retries++ })

		resp2, err2 := r.call(ctx, r.request(job, prep, prep, note, true, truncationScale))
		switch {
		case isFatal(err2):
			return err2
		case truncated(resp2, err2):
			return r.splitGroup(ctx, job, prep, depth, truncationScale, "the retry hit the output-token limit")
		case err2 != nil:
			r.warn("batch %d: retry failed: %v", job.id, err2)
		default:
			parsed, _ := parseJSONL(resp2.Content)
			merge(got, parsed, prep, false)
		}
	}

	// Cause 3: some cues came back and some did not.
	if missing := pick(prep, func(p *prepared) bool { _, ok := got[p.id]; return !ok }); len(missing) > 0 && len(got) > 0 {
		r.warn("batch %d: %d of %d cues missing from the reply; re-requesting only those",
			job.id, len(missing), len(prep))
		r.bump(func(s *Stats) { s.Retries++ })

		note := fmt.Sprintf("Your previous reply omitted %d of the cues. Return ONLY the cue ids listed below, one JSON object per line.", len(missing))
		resp3, err3 := r.call(ctx, r.request(job, prep, missing, note, true, scale))
		switch {
		case isFatal(err3):
			return err3
		case err3 != nil:
			r.warn("batch %d: re-request failed: %v", job.id, err3)
		default:
			parsed, _ := parseJSONL(resp3.Content)
			merge(got, parsed, missing, false)
		}
	}

	// Cause 4 again, at cue granularity this time.
	if refused := pick(prep, func(p *prepared) bool { e, ok := got[p.id]; return ok && e.refused() }); len(refused) > 0 {
		r.bump(func(s *Stats) { s.Refusals += len(refused); s.Retries++ })
		r.warn("batch %d: %d cue(s) refused; retrying them once", job.id, len(refused))

		resp4, err4 := r.call(ctx, r.request(job, prep, refused, refusalNudge, true, scale))
		switch {
		case isFatal(err4):
			return err4
		case err4 != nil:
			r.warn("batch %d: refusal retry failed: %v", job.id, err4)
		default:
			// override: the entries already held for these ids are refusals.
			parsed, _ := parseJSONL(resp4.Content)
			merge(got, parsed, refused, true)
		}
	}

	r.apply(job, prep, got)
	return nil
}

// splitGroup halves a group after a truncated reply.
//
// scale is inherited by both halves. Without it each half started again at the
// base ceiling — half the content, but also half the ceiling that had just
// proven insufficient — and since the shortfall is per-call reasoning overhead
// rather than content, every one of the six resulting calls was certain to
// truncate too.
func (r *runner) splitGroup(ctx context.Context, job batchJob, prep []*prepared, depth, scale int, why string) error {
	if depth >= maxSplitDepth || len(prep) < 2 {
		r.warn("batch %d: %s and the batch cannot be split further; %d cue(s) left untranslated",
			job.id, why, len(prep))
		r.fallback(prep)
		return nil
	}
	r.bump(func(s *Stats) { s.Splits++ })
	r.warn("batch %d: %s; splitting %d cues into two", job.id, why, len(prep))

	mid := len(prep) / 2
	left, right := prep[:mid], prep[mid:]

	// Each half becomes the other's context, so splitting never costs the
	// continuity the ±3 window was there to provide.
	leftJob := batchJob{id: job.id, before: job.before, after: headText(right, contextCues)}
	rightJob := batchJob{id: job.id, before: tailText(left, contextCues), after: job.after}

	if err := r.translateGroup(ctx, leftJob, left, depth+1, scale); err != nil {
		return err
	}
	return r.translateGroup(ctx, rightJob, right, depth+1, scale)
}

// apply validates every reply entry and writes the cues that survived.
func (r *runner) apply(job batchJob, prep []*prepared, got map[int]entry) {
	for _, p := range prep {
		e, ok := got[p.id]
		switch {
		case !ok:
			r.warn("batch %d: cue %d never came back; keeping the original", job.id, p.idx+1)
			r.fallback([]*prepared{p})
			continue
		case e.refused():
			r.warn("batch %d: cue %d refused after a retry; keeping the original", job.id, p.idx+1)
			r.fallback([]*prepared{p})
			continue
		case e.blank():
			r.warn("batch %d: cue %d came back empty; keeping the original", job.id, p.idx+1)
			r.fallback([]*prepared{p})
			continue
		}

		text := []string(e.T)
		if len(text) != p.n() {
			// The line count is the protocol invariant that makes whitespace
			// re-attachment sound. Do not index into a reply of the wrong
			// length: re-wrap it ourselves and say so.
			r.warn("batch %d: cue %d came back as %d line(s) for a %d-line cue; re-split",
				job.id, p.idx+1, len(text), p.n())
			text = balancedSplit(strings.Join(text, " "), p.n())
		}

		if !sameTags(p.src, text) {
			r.warn("batch %d: cue %d lost or invented markup (%s vs %s); keeping the original",
				job.id, p.idx+1, tagList(p.src), tagList(text))
			r.fallback([]*prepared{p})
			continue
		}

		out := make([]string, len(p.parts))
		for i, lp := range p.parts {
			out[i] = lp.rebuild("")
		}
		for k, li := range p.core {
			out[li] = p.parts[li].rebuild(strings.TrimSpace(text[k]))
		}
		r.out[p.idx] = out
	}
}

// fallback records cues that keep their original text. The cue is left as nil
// in r.out, which assemble reads as "clone the input lines".
func (r *runner) fallback(prep []*prepared) {
	r.bump(func(s *Stats) { s.Untranslated += len(prep) })
}

// merge folds parsed entries into got, accepting only ids that were actually
// requested. want is the set the call asked for; override says whether an entry
// already present may be replaced (true only for the refusal retry, where the
// entry being replaced is the refusal itself).
func merge(got map[int]entry, parsed map[int]entry, want []*prepared, override bool) {
	allowed := make(map[int]bool, len(want))
	for _, p := range want {
		allowed[p.id] = true
	}
	for id, e := range parsed {
		if !allowed[id] {
			continue
		}
		if _, exists := got[id]; exists && !override {
			continue
		}
		got[id] = e
	}
}

func pick(prep []*prepared, keep func(*prepared) bool) []*prepared {
	var out []*prepared
	for _, p := range prep {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

// truncated reports the one failure that must never be retried at the same
// size. The client surfaces it two ways: as ErrTruncated when the truncated
// reply was empty, and as a successful response carrying FinishLength when it
// was not.
func truncated(resp llm.Response, err error) bool {
	return errors.Is(err, llm.ErrTruncated) || resp.FinishReason == llm.FinishLength
}

// isFatal reports the errors no fallback can survive.
func isFatal(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrCallBudget) ||
		// The breaker only trips once calls have stopped working entirely, so
		// carrying on would just fail every remaining batch in turn.
		errors.Is(err, ErrProviderUnreachable) ||
		errors.Is(err, ErrStalled) ||
		errors.Is(err, llm.ErrAuth) ||
		errors.Is(err, llm.ErrCreditExhausted) ||
		errors.Is(err, llm.ErrBudgetExceeded)
}

func headText(prep []*prepared, n int) []string {
	var out []string
	for _, p := range prep[:min(n, len(prep))] {
		out = append(out, strings.Join(p.src, " "))
	}
	return out
}

func tailText(prep []*prepared, n int) []string {
	var out []string
	for _, p := range prep[max(0, len(prep)-n):] {
		out = append(out, strings.Join(p.src, " "))
	}
	return out
}

func tagList(lines []string) string {
	m := tagMultiset(strings.Join(lines, "\n"))
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, fmt.Sprintf("%s×%d", k, v))
	}
	slices.Sort(keys)
	return strings.Join(keys, " ")
}

func ptr[T any](v T) *T { return &v }
