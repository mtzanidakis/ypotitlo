package translate

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

func TestRunHappyPath(t *testing.T) {
	t.Parallel()

	in := makeCues(5)
	p := echoing(prefix("EL:"))
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertShape(t, in, res.Cues)
	for i, c := range res.Cues {
		want := fmt.Sprintf("EL:line %d", i+1)
		if len(c.Lines) != 1 || c.Lines[0] != want {
			t.Errorf("cue %d: lines %q, want [%q]", i, c.Lines, want)
		}
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %q", warns)
	}
	if res.Stats.Calls != 1 || res.Stats.Untranslated != 0 || res.Stats.Batches != 1 {
		t.Errorf("stats = %+v", res.Stats)
	}
	// The input must not have been touched.
	if in[0].Lines[0] != "line 1" {
		t.Errorf("input mutated: %q", in[0].Lines[0])
	}
}

// The reply is parsed line by line, so anything that is not a JSON object is
// simply skipped. These are the four ways a model wraps its answer.
func TestRunTolerantReplyEnvelopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wrap func(string) string
	}{
		{"markdown fence", func(s string) string { return "```json\n" + s + "```\n" }},
		{"preamble", func(s string) string { return "Sure! Here are the translated cues:\n\n" + s }},
		{"trailing chatter", func(s string) string { return s + "\nLet me know if you'd like any adjustments!\n" }},
		{"crlf", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
		{"blank lines", func(s string) string { return "\n\n" + strings.ReplaceAll(s, "\n", "\n\n") }},
		{"everything at once", func(s string) string {
			return "Here you go:\n\n```json\r\n" + strings.ReplaceAll(s, "\n", "\r\n") + "```\r\n\r\nHope that helps!"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := makeCues(4)
			p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
				return llm.Response{Content: tc.wrap(reply(req, prefix("EL:")))}, nil
			}}
			var warns []string
			res, err := Run(context.Background(), in, opts(p, &warns))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertShape(t, in, res.Cues)
			for i, c := range res.Cues {
				if want := fmt.Sprintf("EL:line %d", i+1); c.Lines[0] != want {
					t.Fatalf("cue %d: %q, want %q", i, c.Lines[0], want)
				}
			}
			if res.Stats.Calls != 1 {
				t.Errorf("calls = %d, want 1 (envelope noise must not trigger a repair)", res.Stats.Calls)
			}
		})
	}
}

// One malformed object costs one cue, not the batch — and the repair asks only
// for that cue.
func TestRunMalformedLineCostsOneCue(t *testing.T) {
	t.Parallel()

	in := makeCues(5)
	var second []int
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n > 1 {
			second = wantedIDs(req)
			// The repair fails too, so the damage is visible in the result.
			return llm.Response{Content: "still broken"}, nil
		}
		var sb strings.Builder
		for _, c := range parseRequest(req) {
			if c.id == 3 {
				sb.WriteString(`{"i":3,"n":1,"t":["EL:line 3"` + "\n") // truncated object
				continue
			}
			sb.WriteString(jsonLine(c.id, c.n, mapAll(c.lines, prefix("EL:")), "ok") + "\n")
		}
		return llm.Response{Content: sb.String()}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	for i, c := range res.Cues {
		want := fmt.Sprintf("EL:line %d", i+1)
		if i == 2 {
			want = "line 3"
		}
		if c.Lines[0] != want {
			t.Errorf("cue %d: %q, want %q", i, c.Lines[0], want)
		}
	}
	if res.Stats.Untranslated != 1 {
		t.Errorf("untranslated = %d, want 1", res.Stats.Untranslated)
	}
	if !slices.Equal(second, []int{3}) {
		t.Errorf("repair asked for ids %v, want [3]", second)
	}
	if !hasWarning(warns, "missing from the reply") {
		t.Errorf("warnings = %q", warns)
	}
}

// The partial path is the common one with JSON Lines: re-request the missing
// ids only, do not halve the batch and do not re-translate what arrived.
func TestRunMissingCuesReRequested(t *testing.T) {
	t.Parallel()

	in := makeCues(6)
	var asked []int
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n > 1 {
			asked = wantedIDs(req)
			return llm.Response{Content: reply(req, prefix("EL2:"))}, nil
		}
		var sb strings.Builder
		for _, c := range parseRequest(req) {
			if c.id == 2 || c.id == 5 {
				continue
			}
			sb.WriteString(jsonLine(c.id, c.n, mapAll(c.lines, prefix("EL:")), "ok") + "\n")
		}
		return llm.Response{Content: sb.String()}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if !slices.Equal(asked, []int{2, 5}) {
		t.Fatalf("re-request asked for %v, want [2 5]", asked)
	}
	for i, c := range res.Cues {
		want := fmt.Sprintf("EL:line %d", i+1)
		if i == 1 || i == 4 {
			want = fmt.Sprintf("EL2:line %d", i+1)
		}
		if c.Lines[0] != want {
			t.Errorf("cue %d: %q, want %q", i, c.Lines[0], want)
		}
	}
	if res.Stats.Calls != 2 || res.Stats.Retries != 1 || res.Stats.Splits != 0 {
		t.Errorf("stats = %+v; the partial path must cost one call and no split", res.Stats)
	}
	if res.Stats.Untranslated != 0 {
		t.Errorf("untranslated = %d, want 0", res.Stats.Untranslated)
	}
}

// A reply whose line count disagrees with the source must never be indexed
// blindly. It is re-split by us, and said so.
func TestRunLineCountMismatchIsReSplit(t *testing.T) {
	t.Parallel()

	in := []srt.Cue{cue("1", 0, 2000, "Where are you going?", "I told you already.")}
	p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
		c := parseRequest(req)[0]
		// One line where the source had two.
		return llm.Response{Content: jsonLine(c.id, 1, []string{"Πού πηγαίνεις; Σου το είπα ήδη."}, "ok")}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	got := res.Cues[0].Lines
	if len(got) != 2 {
		t.Fatalf("lines = %q, want 2", got)
	}
	if strings.Join(got, " ") != "Πού πηγαίνεις; Σου το είπα ήδη." {
		t.Errorf("re-split lost text: %q", got)
	}
	if len([]rune(got[0])) > len([]rune(got[1])) {
		t.Errorf("first line %q is longer than second %q; the break should be bottom-heavy", got[0], got[1])
	}
	if !hasWarning(warns, "re-split") {
		t.Errorf("warnings = %q", warns)
	}
	if res.Stats.Untranslated != 0 {
		t.Errorf("untranslated = %d, want 0: a re-split cue is still translated", res.Stats.Untranslated)
	}
}

func TestRunRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		relent       bool
		wantLine     string
		untranslated int
	}{
		{"model relents on the second ask", true, "EL2:line 2", 0},
		{"model refuses twice", false, "line 2", 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := makeCues(4)
			p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
				var sb strings.Builder
				for _, c := range parseRequest(req) {
					if !slices.Contains(wantedIDs(req), c.id) {
						continue
					}
					switch {
					case c.id != 2:
						sb.WriteString(jsonLine(c.id, c.n, mapAll(c.lines, prefix("EL:")), "ok") + "\n")
					case n > 1 && tc.relent:
						sb.WriteString(jsonLine(c.id, c.n, mapAll(c.lines, prefix("EL2:")), "ok") + "\n")
					default:
						sb.WriteString(jsonLine(c.id, c.n, nil, "refused") + "\n")
					}
				}
				return llm.Response{Content: sb.String()}, nil
			}}

			var warns []string
			res, err := Run(context.Background(), in, opts(p, &warns))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertShape(t, in, res.Cues)

			if got := res.Cues[1].Lines[0]; got != tc.wantLine {
				t.Errorf("cue 2 = %q, want %q", got, tc.wantLine)
			}
			if res.Stats.Refusals != 1 {
				t.Errorf("refusals = %d, want 1", res.Stats.Refusals)
			}
			if res.Stats.Untranslated != tc.untranslated {
				t.Errorf("untranslated = %d, want %d", res.Stats.Untranslated, tc.untranslated)
			}
			// One extra call, not a cascade.
			if res.Stats.Calls != 2 {
				t.Errorf("calls = %d, want 2", res.Stats.Calls)
			}
			// The retry must name the material, and must ask for the refused
			// cue only.
			reqs := p.requests()
			if !strings.Contains(userMessage(reqs[1]), "published dialogue") {
				t.Errorf("refusal retry did not carry the nudge:\n%s", userMessage(reqs[1]))
			}
			if !slices.Equal(wantedIDs(reqs[1]), []int{2}) {
				t.Errorf("refusal retry asked for %v, want [2]", wantedIDs(reqs[1]))
			}
			if !hasWarning(warns, "refused") {
				t.Errorf("warnings = %q", warns)
			}
		})
	}
}

// A prose refusal with no JSON at all is still a refusal, not a format error.
func TestRunProseRefusal(t *testing.T) {
	t.Parallel()

	in := makeCues(3)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n == 1 {
			return llm.Response{Content: "I'm sorry, but I can't help with translating this content."}, nil
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)
	if res.Stats.Refusals != 3 {
		t.Errorf("refusals = %d, want 3", res.Stats.Refusals)
	}
	if !strings.Contains(userMessage(p.requests()[1]), "published dialogue") {
		t.Error("the retry after a prose refusal must carry the nudge, not a format complaint")
	}
	for i, c := range res.Cues {
		if want := fmt.Sprintf("EL:line %d", i+1); c.Lines[0] != want {
			t.Errorf("cue %d: %q, want %q", i, c.Lines[0], want)
		}
	}
}

// A reply that parsed as nothing at all gets exactly one strict retry, at
// temperature 0, naming the violation.
func TestRunFormatViolationRetriesOnceAtTemperatureZero(t *testing.T) {
	t.Parallel()

	in := makeCues(3)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n == 1 {
			return llm.Response{Content: "1. Γεια σου\n2. Αντίο\n3. Τέλος\n"}, nil
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	reqs := p.requests()
	if len(reqs) != 2 {
		t.Fatalf("calls = %d, want 2", len(reqs))
	}
	if reqs[1].Temperature == nil || *reqs[1].Temperature != 0 {
		t.Errorf("retry temperature = %v, want 0", reqs[1].Temperature)
	}
	if !strings.Contains(userMessage(reqs[1]), "not a JSON object") {
		t.Errorf("retry did not name the violation:\n%s", userMessage(reqs[1]))
	}
	if res.Stats.Retries != 1 || res.Stats.Splits != 0 {
		t.Errorf("stats = %+v", res.Stats)
	}
}

// A truncated reply raises the ceiling before it shrinks the batch.
//
// This is not a preference, it is the difference between a run that finishes
// and one that does not. On a reasoning model the thinking is billed against
// max_tokens and its size tracks the task, not the number of cues, so halving
// the batch leaves the overhead exactly where it was. A real run against
// deepseek-v4-pro did precisely that: batches went 20 -> 10 -> 5 cues,
// truncated at every size, and burned the call fuse without producing a file.
func TestRunTruncationRaisesTheCeilingBeforeSplitting(t *testing.T) {
	t.Parallel()

	in := makeCues(6)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n == 1 {
			return llm.Response{FinishReason: llm.FinishLength, Content: `{"i":1,"n":1,"t":["EL:line 1"],"s":"ok"}`}, nil
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if res.Stats.Splits != 0 {
		t.Errorf("splits = %d, want 0: raising the ceiling should have been enough", res.Stats.Splits)
	}
	if res.Stats.Calls != 2 {
		t.Errorf("calls = %d, want 2 (one truncated, one at a higher ceiling)", res.Stats.Calls)
	}

	reqs := p.requests()
	if len(reqs) < 2 {
		t.Fatalf("requests = %d, want at least 2", len(reqs))
	}
	if reqs[1].MaxTokens <= reqs[0].MaxTokens {
		t.Errorf("retry ceiling = %d, not above the first call's %d", reqs[1].MaxTokens, reqs[0].MaxTokens)
	}
	if got := len(wantedIDs(reqs[1])); got != 6 {
		t.Errorf("the retry asked for %d cues, want all 6: the batch must not have been split", got)
	}
	for i, c := range res.Cues {
		if want := fmt.Sprintf("EL:line %d", i+1); c.Lines[0] != want {
			t.Errorf("cue %d: %q, want %q", i, c.Lines[0], want)
		}
	}
	if !hasWarning(warns, "higher one") {
		t.Errorf("warnings = %q", warns)
	}
}

// When even the raised ceiling truncates, the batch is finally halved.
func TestRunSplitsWhenARaisedCeilingStillTruncates(t *testing.T) {
	t.Parallel()

	in := makeCues(6)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		// Both the first call and the raised-ceiling retry come back cut off;
		// only the halves succeed.
		if n <= 2 {
			return llm.Response{FinishReason: llm.FinishLength}, nil
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if res.Stats.Splits != 1 {
		t.Errorf("splits = %d, want 1", res.Stats.Splits)
	}
	reqs := p.requests()
	if got := len(wantedIDs(reqs[2])) + len(wantedIDs(reqs[3])); got != 6 {
		t.Errorf("the two halves covered %d cues, want 6", got)
	}
	if !hasWarning(warns, "splitting") {
		t.Errorf("warnings = %q", warns)
	}
}

// The truncated reply may also arrive as an error with no content at all, and
// must be recognised as a token-ceiling problem rather than a failed call.
func TestRunTruncationErrorIsRecognised(t *testing.T) {
	t.Parallel()

	in := makeCues(4)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		if n == 1 {
			return llm.Response{FinishReason: llm.FinishLength}, fmt.Errorf("%w (finish_reason=length)", llm.ErrTruncated)
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if res.Stats.Calls != 2 {
		t.Errorf("calls = %d, want 2 (one truncated, one at a higher ceiling)", res.Stats.Calls)
	}
	reqs := p.requests()
	if reqs[1].MaxTokens <= reqs[0].MaxTokens {
		t.Errorf("retry ceiling = %d, not above the first call's %d", reqs[1].MaxTokens, reqs[0].MaxTokens)
	}
}

// Splitting bottoms out rather than recursing forever, and the cues that are
// left over keep their original text.
func TestRunSplitDepthIsCapped(t *testing.T) {
	t.Parallel()

	in := makeCues(5)
	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		return llm.Response{FinishReason: llm.FinishLength}, llm.ErrTruncated
	}}

	var warns []string
	o := opts(p, &warns)
	o.MaxCalls = 100
	res, err := Run(context.Background(), in, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if res.Stats.Untranslated != 5 {
		t.Errorf("untranslated = %d, want 5", res.Stats.Untranslated)
	}
	for i, c := range res.Cues {
		if want := fmt.Sprintf("line %d", i+1); c.Lines[0] != want {
			t.Errorf("cue %d: %q, want the original %q", i, c.Lines[0], want)
		}
	}
	// Each level now spends one extra call raising the ceiling before it gives
	// up and halves, so the ceiling is higher than it was — but it is still a
	// small constant, which is the property under test: a wedged batch must not
	// cascade into an unbounded number of calls.
	if res.Stats.Calls > 20 {
		t.Errorf("calls = %d; a capped split must not cascade", res.Stats.Calls)
	}
	if !hasWarning(warns, "cannot be split further") {
		t.Errorf("warnings = %q", warns)
	}
}

// The fuse is absolute: exceeding it aborts with a clear error rather than
// spending unbounded money.
func TestRunCallBudgetFuse(t *testing.T) {
	t.Parallel()

	in := makeCues(6)
	p := echoing(prefix("EL:"))

	var warns []string
	o := opts(p, &warns)
	o.BatchSize = 2
	o.Concurrency = 1
	o.MaxCalls = 2

	res, err := Run(context.Background(), in, o)
	if !errors.Is(err, ErrCallBudget) {
		t.Fatalf("err = %v, want ErrCallBudget", err)
	}
	if res.Cues != nil {
		t.Errorf("cues = %v, want nil on a fatal error", res.Cues)
	}
	if p.count() != 2 {
		t.Errorf("provider calls = %d, want 2", p.count())
	}
	if res.Stats.Calls != 2 {
		t.Errorf("stats.Calls = %d, want 2", res.Stats.Calls)
	}
}

func TestRunDefaultCallBudget(t *testing.T) {
	t.Parallel()

	// 100 cues at the default batch size of 20 is 5 batches, so the default
	// fuse is 3*5+10 = 25.
	r := newRunner(Options{})
	r.budget(100)
	if r.maxCalls != 25 {
		t.Errorf("maxCalls = %d, want 25", r.maxCalls)
	}
}

// Batches run concurrently; the output must not depend on which finished first.
func TestRunConcurrencyPreservesOrder(t *testing.T) {
	t.Parallel()

	const n = 61
	in := makeCues(n)
	p := echoing(prefix("EL:"))

	var warns []string
	o := opts(p, &warns)
	o.BatchSize = 5
	o.Concurrency = 4

	var progress [][2]int
	var pmu = make(chan struct{}, 1)
	pmu <- struct{}{}
	o.Progress = func(done, total int) {
		<-pmu
		progress = append(progress, [2]int{done, total})
		pmu <- struct{}{}
	}

	res, err := Run(context.Background(), in, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)
	for i, c := range res.Cues {
		if want := fmt.Sprintf("EL:line %d", i+1); c.Lines[0] != want {
			t.Fatalf("cue %d out of order: %q, want %q", i, c.Lines[0], want)
		}
	}
	if len(progress) != res.Stats.Batches {
		t.Errorf("progress called %d times, want %d", len(progress), res.Stats.Batches)
	}
	if last := progress[len(progress)-1]; last[0] != n || last[1] != n {
		t.Errorf("final progress = %v, want [%d %d]", last, n, n)
	}
}

func TestRunEmptyInputMakesNoCalls(t *testing.T) {
	t.Parallel()

	p := echoing(prefix("EL:"))
	var warns []string
	o := opts(p, &warns)
	o.Brief = true

	res, err := Run(context.Background(), nil, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Cues) != 0 {
		t.Errorf("cues = %v, want empty", res.Cues)
	}
	if p.count() != 0 {
		t.Errorf("provider calls = %d, want 0", p.count())
	}
}

// Cues with nothing to translate are never sent, and come back untouched.
func TestRunSkipsUntranslatableCues(t *testing.T) {
	t.Parallel()

	in := []srt.Cue{
		cue("1", 0, 1000),
		cue("2", 1000, 2000, "   "),
		cue("3", 2000, 3000, `{\an8}`),
		cue("4", 3000, 4000, "Hello"),
	}
	p := echoing(prefix("EL:"))
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	if got := parseRequest(p.requests()[0]); len(got) != 1 || got[0].lines[0] != "Hello" {
		t.Errorf("request carried %+v, want only the one translatable cue", got)
	}
	for i, want := range [][]string{{}, {"   "}, {`{\an8}`}, {"EL:Hello"}} {
		if !slices.Equal(res.Cues[i].Lines, want) {
			t.Errorf("cue %d: %q, want %q", i, res.Cues[i].Lines, want)
		}
	}
	if res.Stats.Untranslated != 0 {
		t.Errorf("untranslated = %d, want 0: a cue with no text is not a failure", res.Stats.Untranslated)
	}
}

func TestRunValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mut  func(o *Options)
		want string
	}{
		{"no provider", func(o *Options) { o.Provider = nil }, "no provider"},
		{"no target", func(o *Options) { o.Target = greek(); o.Target.Code = "" }, "no target language"},
		{"no model", func(o *Options) { o.Model = "" }, "no model"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var warns []string
			o := opts(echoing(prefix("EL:")), &warns)
			tc.mut(&o)
			_, err := Run(context.Background(), makeCues(2), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// A cancelled context must never produce a result the caller could mistake for
// a finished translation.
func TestRunCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var warns []string
	_, err := Run(ctx, makeCues(4), opts(echoing(prefix("EL:")), &warns))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// An auth failure is fatal: no fallback can help and every further call will
// fail the same way.
func TestRunFatalProviderErrors(t *testing.T) {
	t.Parallel()

	for _, target := range []error{llm.ErrAuth, llm.ErrCreditExhausted, llm.ErrBudgetExceeded} {
		t.Run(target.Error(), func(t *testing.T) {
			t.Parallel()

			p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
				return llm.Response{}, fmt.Errorf("call: %w", target)
			}}
			var warns []string
			o := opts(p, &warns)
			o.Concurrency = 1
			_, err := Run(context.Background(), makeCues(4), o)
			if !errors.Is(err, target) {
				t.Fatalf("err = %v, want %v", err, target)
			}
		})
	}
}

// A call that fails for an ordinary reason is retried once and then falls back.
func TestRunTransportFailureFallsBack(t *testing.T) {
	t.Parallel()

	in := makeCues(3)
	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		return llm.Response{}, errors.New("connection reset")
	}}
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)
	if res.Stats.Untranslated != 3 {
		t.Errorf("untranslated = %d, want 3", res.Stats.Untranslated)
	}
	if res.Stats.Calls != 2 {
		t.Errorf("calls = %d, want 2 (one call plus one retry)", res.Stats.Calls)
	}
	if !hasWarning(warns, "connection reset") {
		t.Errorf("warnings = %q", warns)
	}
	if len(res.Warnings) != len(warns) {
		t.Errorf("Result.Warnings (%d) and the Warn seam (%d) disagree", len(res.Warnings), len(warns))
	}
}

// Sampling and token budget: the request must not run at the provider default
// temperature, must ask reasoning models not to think, and must size its own
// output ceiling.
func TestRequestSampling(t *testing.T) {
	t.Parallel()

	p := echoing(prefix("EL:"))
	var warns []string
	if _, err := Run(context.Background(), makeCues(3), opts(p, &warns)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := p.requests()[0]
	if req.Temperature == nil || *req.Temperature != tempNormal {
		t.Errorf("temperature = %v, want %v", req.Temperature, tempNormal)
	}
	if req.ReasoningEffort != reasoningEffort {
		t.Errorf("reasoning effort = %q, want %q", req.ReasoningEffort, reasoningEffort)
	}
	if req.MaxTokens < minBatchTokens || req.MaxTokens > maxBatchTokens {
		t.Errorf("max tokens = %d, outside [%d,%d]", req.MaxTokens, minBatchTokens, maxBatchTokens)
	}
	if req.Stage != "batch" || req.Model != "test-model" {
		t.Errorf("stage/model = %q/%q", req.Stage, req.Model)
	}
}

// TestCeilingLeavesRoomToThink pins the constant that wedged a real run.
//
// A reasoning model bills its thinking against max_tokens and spends it before
// emitting anything: measured against deepseek-v4-pro, "hi" with a ceiling of
// 50 returned finish_reason=length and an empty message, and a 20-cue batch
// spends on the order of ten thousand tokens thinking. Because that cost is per
// call rather than per cue, a ceiling sized from the source text truncates
// every batch however small, and halving does not rescue it.
func TestCeilingLeavesRoomToThink(t *testing.T) {
	t.Parallel()

	// Even a one-cue batch, whose content needs almost nothing, must carry
	// enough room for the thinking that precedes it.
	tiny := maxTokensFor([]*prepared{{src: []string{"Hi."}}}, 1)
	if tiny < reasoningHeadroom {
		t.Errorf("ceiling for a one-cue batch = %d, below the reasoning headroom %d", tiny, reasoningHeadroom)
	}
}

func TestCeilingGrowsWithSourceSizeAndIsCapped(t *testing.T) {
	t.Parallel()

	small := maxTokensFor([]*prepared{{src: []string{"Hi."}}}, 1)
	big := maxTokensFor([]*prepared{{src: []string{strings.Repeat("a longer line of dialogue ", 500)}}}, 1)
	if small > big {
		t.Errorf("ceiling shrank as the source grew: %d vs %d", small, big)
	}
	if big > maxBatchTokens {
		t.Errorf("ceiling = %d, above the cap %d: a runaway reasoner must stay bounded", big, maxBatchTokens)
	}
}

// A retry after a truncation does send one, because the model's own default has
// just proven insufficient.
func TestRetryCeilingIsExplicitAndLarge(t *testing.T) {
	t.Parallel()

	prep := []*prepared{{src: []string{"Some dialogue to translate."}}}
	raised := maxTokensFor(prep, truncationScale)

	if raised < minBatchTokens {
		t.Errorf("retry ceiling = %d, below the floor %d", raised, minBatchTokens)
	}
	if raised > maxBatchTokens {
		t.Errorf("retry ceiling = %d, above the cap %d", raised, maxBatchTokens)
	}
}

// Context cues are shown but never requested.
func TestRequestCarriesSourceContext(t *testing.T) {
	t.Parallel()

	in := makeCues(12)
	p := echoing(prefix("EL:"))
	var warns []string
	o := opts(p, &warns)
	o.BatchSize = 4
	o.Concurrency = 1

	if _, err := Run(context.Background(), in, o); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := p.requests()
	if len(reqs) != 3 {
		t.Fatalf("calls = %d, want 3", len(reqs))
	}
	mid := userMessage(reqs[1])
	if !strings.Contains(mid, "CONTEXT BEFORE") || !strings.Contains(mid, "CONTEXT AFTER") {
		t.Fatalf("middle batch has no context:\n%s", mid)
	}
	if !strings.Contains(mid, "  line 2\n") || !strings.Contains(mid, "  line 11\n") {
		t.Errorf("context window is not ±3 source cues:\n%s", mid)
	}
	if got := len(parseRequest(reqs[1])); got != 4 {
		t.Errorf("emitted-cue blocks = %d, want 4: context must not be requestable", got)
	}
}

// Sampling seeds come from the injected source, so a run is reproducible, and
// they differ per call, so a temperature-0 retry is not asked to reproduce the
// reply that caused it.
func TestRequestSeedsAreInjectedAndDistinct(t *testing.T) {
	t.Parallel()

	run := func() []int {
		p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
			if n == 1 {
				return llm.Response{Content: "not json at all"}, nil
			}
			return llm.Response{Content: reply(req, prefix("EL:"))}, nil
		}}
		var warns []string
		o := opts(p, &warns)
		o.Rand = rand.New(rand.NewSource(42))
		if _, err := Run(context.Background(), makeCues(3), o); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var seeds []int
		for _, req := range p.requests() {
			if req.Seed == nil {
				t.Fatal("request carried no seed")
			}
			seeds = append(seeds, *req.Seed)
		}
		return seeds
	}

	a, b := run(), run()
	if !slices.Equal(a, b) {
		t.Errorf("seeds are not reproducible: %v vs %v", a, b)
	}
	if len(a) != 2 {
		t.Fatalf("got %d calls, want 2", len(a))
	}
	if a[0] == a[1] {
		t.Errorf("the retry re-used the first call's seed (%d)", a[0])
	}
}

// Token and cost accounting is folded in from every call, including the ones
// that failed: a call that errored after three HTTP retries still cost money.
func TestStatsAccounting(t *testing.T) {
	t.Parallel()

	in := makeCues(4)
	p := &fakeProvider{fn: func(req llm.Request, n int) (llm.Response, error) {
		resp := llm.Response{
			Content: reply(req, prefix("EL:")),
			Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.001, CostKnown: true},
			Retries: 2,
		}
		if n == 1 {
			// A failed first call whose usage still has to be counted.
			resp.Content = ""
			resp.Usage = llm.Usage{PromptTokens: 100, CompletionTokens: 0, CostKnown: false}
			return resp, errors.New("upstream 500")
		}
		return resp, nil
	}}

	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)

	want := Stats{
		Calls: 2, Retries: 1, ProviderRetries: 4, Batches: 1,
		PromptTokens: 200, CompletionTokens: 50, CostUSD: 0.001, UnknownCost: 1,
	}
	if res.Stats != want {
		t.Errorf("stats = %+v, want %+v", res.Stats, want)
	}
}

func TestGuardedProviderName(t *testing.T) {
	t.Parallel()

	var warns []string
	r := newRunner(opts(echoing(prefix("EL:")), &warns))
	if got := (guarded{r}).Name(); got != "fake" {
		t.Errorf("Name = %q, want %q", got, "fake")
	}
}

// TestRunAbortsWhenTheProviderStopsAnswering pins the behaviour that a DNS
// outage exposed. Every batch failed, every failure was warned about, every cue
// quietly kept its source text, and the run would have exited successfully
// having written an untranslated file. A run that cannot reach the provider
// must fail, not degrade.
func TestRunAbortsWhenTheProviderStopsAnswering(t *testing.T) {
	t.Parallel()

	in := makeCues(200)
	var calls atomic.Int32
	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		calls.Add(1)
		return llm.Response{}, fmt.Errorf("dial tcp: lookup opencode.ai: no such host")
	}}

	var warns []string
	o := opts(p, &warns)
	o.Brief = false
	_, err := Run(context.Background(), in, o)

	if !errors.Is(err, ErrProviderUnreachable) {
		t.Fatalf("Run error = %v, want ErrProviderUnreachable", err)
	}
	// It must give up quickly rather than working through every batch.
	if n := calls.Load(); n > maxConsecutiveFailures*3 {
		t.Errorf("made %d calls before giving up; the breaker should trip near %d",
			n, maxConsecutiveFailures)
	}
}

// A single failing batch must not trip the breaker: the counter resets on any
// call that works.
func TestRunToleratesIsolatedFailures(t *testing.T) {
	t.Parallel()

	in := makeCues(60)
	var n atomic.Int32
	p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
		// Fail every third call, succeed otherwise.
		if n.Add(1)%3 == 0 {
			return llm.Response{}, fmt.Errorf("transient")
		}
		return llm.Response{Content: reply(req, prefix("EL:"))}, nil
	}}

	var warns []string
	o := opts(p, &warns)
	o.Brief = false
	res, err := Run(context.Background(), in, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)
}

// TestRunRejectsAMostlyUntranslatedResult covers the other half of the same
// problem: calls that succeed but return nothing usable.
func TestRunRejectsAMostlyUntranslatedResult(t *testing.T) {
	t.Parallel()

	in := makeCues(40)
	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		// Well-formed, entirely useless: no cue objects at all.
		return llm.Response{Content: "I am unable to help with that."}, nil
	}}

	var warns []string
	o := opts(p, &warns)
	o.Brief = false
	res, err := Run(context.Background(), in, o)

	if !errors.Is(err, ErrMostlyUntranslated) {
		t.Fatalf("Run error = %v, want ErrMostlyUntranslated", err)
	}
	// The cues still come back intact so a caller can inspect them.
	assertShape(t, in, res.Cues)
}

// A small file is exempt: one honest fallback in a three-cue file is a third of
// it and says nothing about whether the run worked.
func TestSmallFilesAreExemptFromTheRatioCheck(t *testing.T) {
	t.Parallel()

	in := makeCues(3)
	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		return llm.Response{Content: "no objects here"}, nil
	}}

	var warns []string
	o := opts(p, &warns)
	o.Brief = false
	if _, err := Run(context.Background(), in, o); err != nil {
		t.Fatalf("a %d-cue file must not trip the ratio check: %v", len(in), err)
	}
}
