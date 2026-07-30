package llm

import (
	"fmt"
	"strings"
	"sync"
)

// Tally accumulates per-call usage so the CLI can print one run footer at the
// end. Safe for concurrent use: translation runs several workers against a
// single client.
type Tally struct {
	mu sync.Mutex
	t  Totals
}

// Totals is a snapshot of a Tally.
type Totals struct {
	Calls            int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	// UnknownCost counts the calls whose model is not in the price table and
	// therefore contributed nothing to CostUSD. It exists so the footer can say
	// so rather than quietly under-report the bill.
	UnknownCost int
	Retries     int
}

// Add records one call. It takes the Response rather than the Usage because a
// failed call still costs retries, and a Response carries them even when the
// call returned an error.
func (t *Tally) Add(resp Response) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.t.Calls++
	t.t.PromptTokens += resp.Usage.PromptTokens
	t.t.CompletionTokens += resp.Usage.CompletionTokens
	t.t.Retries += resp.Retries
	if resp.Usage.CostKnown {
		t.t.CostUSD += resp.Usage.CostUSD
		return
	}
	// A call with no tokens reported (a hard failure) is not a call of unknown
	// cost; it is a call of no cost.
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		t.t.UnknownCost++
	}
}

// Totals returns a snapshot.
func (t *Tally) Totals() Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.t
}

// String renders the run footer, e.g.
//
//	17 calls · 21430 in / 34880 out · ~$0.009 · 4 retries
func (t Totals) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s · %d in / %d out · ",
		t.Calls, plural(t.Calls, "call"), t.PromptTokens, t.CompletionTokens)
	switch {
	case t.UnknownCost == t.Calls && t.Calls > 0:
		b.WriteString("cost unknown")
	case t.UnknownCost > 0:
		fmt.Fprintf(&b, "~$%.3f + %d %s of unknown cost", t.CostUSD, t.UnknownCost, plural(t.UnknownCost, "call"))
	default:
		fmt.Fprintf(&b, "~$%.3f", t.CostUSD)
	}
	fmt.Fprintf(&b, " · %d %s", t.Retries, plural(t.Retries, "retry"))
	return b.String()
}

// plural is the crudest possible pluralizer; it only ever sees "call" and
// "retry".
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	if strings.HasSuffix(word, "y") {
		return strings.TrimSuffix(word, "y") + "ies"
	}
	return word + "s"
}
