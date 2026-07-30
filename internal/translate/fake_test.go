package translate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// fakeProvider is the only llm.Provider these tests ever see. Nothing in this
// package may reach the network, and nothing here can: there is no http.Client
// anywhere in the test binary's use of internal/translate.
type fakeProvider struct {
	fn func(req llm.Request, n int) (llm.Response, error)

	mu   sync.Mutex
	reqs []llm.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	n := len(f.reqs)
	f.mu.Unlock()
	return f.fn(req, n)
}

func (f *fakeProvider) requests() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Request(nil), f.reqs...)
}

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// echoing builds a provider that answers every batch request correctly, running
// each source line through transform.
func echoing(transform func(string) string) *fakeProvider {
	return &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
		return llm.Response{Content: reply(req, transform)}, nil
	}}
}

// reqCue is one "#id n=N" block recovered from a request.
type reqCue struct {
	id    int
	n     int
	lines []string
}

// parseRequest recovers the cue blocks from the rendered user message. Doing it
// this way rather than reaching into unexported builders keeps the tests honest
// about the wire: if the request format changes in a way a model could not
// follow, these helpers stop working too.
func parseRequest(req llm.Request) []reqCue {
	var out []reqCue
	lines := strings.Split(userMessage(req), "\n")
	for i := 0; i < len(lines); i++ {
		id, n, ok := parseHeader(lines[i])
		if !ok {
			continue
		}
		c := reqCue{id: id, n: n}
		for j := 1; j <= n && i+j < len(lines); j++ {
			c.lines = append(c.lines, lines[i+j])
		}
		i += n
		out = append(out, c)
	}
	return out
}

func parseHeader(s string) (id, n int, ok bool) {
	if !strings.HasPrefix(s, "#") {
		return 0, 0, false
	}
	rest, found := strings.CutPrefix(s, "#")
	if !found {
		return 0, 0, false
	}
	idStr, nStr, found := strings.Cut(rest, " n=")
	if !found {
		return 0, 0, false
	}
	id, err1 := strconv.Atoi(idStr)
	n, err2 := strconv.Atoi(nStr)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return id, n, true
}

// wantedIDs recovers the ids the request asked to be emitted.
func wantedIDs(req llm.Request) []int {
	const marker = "for these cue ids exactly: "
	_, rest, ok := strings.Cut(userMessage(req), marker)
	if !ok {
		return nil
	}
	rest, _, _ = strings.Cut(rest, "\n")
	var out []int
	for _, f := range strings.Split(rest, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func userMessage(req llm.Request) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			return m.Content
		}
	}
	return ""
}

func systemMessage(req llm.Request) string {
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			return m.Content
		}
	}
	return ""
}

// reply renders a well-formed JSON Lines answer to req.
func reply(req llm.Request, transform func(string) string) string {
	want := map[int]bool{}
	for _, id := range wantedIDs(req) {
		want[id] = true
	}
	var sb strings.Builder
	for _, c := range parseRequest(req) {
		if !want[c.id] {
			continue
		}
		sb.WriteString(jsonLine(c.id, c.n, mapAll(c.lines, transform), "ok"))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func mapAll(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

// jsonLine renders one reply object. It is built by hand rather than with
// encoding/json so that a test can emit a deliberately malformed one.
func jsonLine(id, n int, lines []string, status string) string {
	quoted := make([]string, len(lines))
	for i, s := range lines {
		quoted[i] = strconv.Quote(s)
	}
	return fmt.Sprintf(`{"i":%d,"n":%d,"t":[%s],"s":%q}`, id, n, strings.Join(quoted, ","), status)
}

func prefix(p string) func(string) string {
	return func(s string) string { return p + s }
}

// --- cue helpers ---------------------------------------------------------

func cue(index string, startMs, endMs int, lines ...string) srt.Cue {
	if lines == nil {
		lines = []string{}
	}
	return srt.Cue{
		Index: index,
		Start: time.Duration(startMs) * time.Millisecond,
		End:   time.Duration(endMs) * time.Millisecond,
		Lines: lines,
	}
}

// makeCues builds n one-line cues two seconds apart, with no scene gaps.
func makeCues(n int) []srt.Cue {
	out := make([]srt.Cue, n)
	for i := range out {
		out[i] = cue(strconv.Itoa(i+1), i*2000, i*2000+1500, fmt.Sprintf("line %d", i+1))
	}
	return out
}

func greek() lang.Lang {
	l, err := lang.Resolve("el")
	if err != nil {
		panic(err)
	}
	return l
}

func english() lang.Lang {
	l, err := lang.Resolve("en")
	if err != nil {
		panic(err)
	}
	return l
}

// opts is the base Options for a test run: no brief, deterministic, warnings
// collected rather than printed.
func opts(p llm.Provider, warns *[]string) Options {
	var mu sync.Mutex
	return Options{
		Provider: p,
		Model:    "test-model",
		Source:   english(),
		Target:   greek(),
		Warn: func(format string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			*warns = append(*warns, fmt.Sprintf(format, a...))
		},
	}
}

// --- assertions ----------------------------------------------------------

// assertShape is the package's central invariant: same count, same order, same
// timings, same indices. Every test that runs a translation calls it.
func assertShape(t *testing.T, in, out []srt.Cue) {
	t.Helper()
	if len(out) != len(in) {
		t.Fatalf("cue count: got %d, want %d", len(out), len(in))
	}
	for i := range in {
		switch {
		case out[i].Index != in[i].Index:
			t.Errorf("cue %d: index %q, want %q", i, out[i].Index, in[i].Index)
		case out[i].Start != in[i].Start:
			t.Errorf("cue %d: start %v, want %v", i, out[i].Start, in[i].Start)
		case out[i].End != in[i].End:
			t.Errorf("cue %d: end %v, want %v", i, out[i].End, in[i].End)
		}
	}
}

func hasWarning(warns []string, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
