package llm

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBudgetRollover(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	b := NewBudgetGuard(1.0, func() time.Time { return day }, time.UTC)
	b.Add(0.9)
	if !b.Allow() {
		t.Error("should allow at 0.9/1.0")
	}
	b.Add(0.2)
	if b.Allow() {
		t.Error("should block at 1.1/1.0")
	}
	day = day.AddDate(0, 0, 1)
	if !b.Allow() {
		t.Error("should allow after day rollover")
	}
	if b.Spent() != 0 {
		t.Errorf("spent after rollover = %v, want 0", b.Spent())
	}
}

func TestBudgetRolloverAtUTCMidnight(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 23, 59, 59, 0, time.UTC)
	b := NewBudgetGuard(1.0, func() time.Time { return now }, time.UTC)
	b.Add(1.5)
	if b.Allow() {
		t.Fatal("should block at 1.5/1.0")
	}
	now = now.Add(time.Second) // 2026-07-31T00:00:00Z
	if !b.Allow() {
		t.Error("the budget is daily; it must reset at UTC midnight")
	}
	if b.Remaining() != 1.0 {
		t.Errorf("remaining = %v, want the full limit back", b.Remaining())
	}
}

func TestBudgetUnlimited(t *testing.T) {
	t.Parallel()
	b := NewBudgetGuard(0, nil, nil)
	b.Add(1e6)
	if !b.Allow() {
		t.Error("a non-positive limit means unlimited")
	}
	if b.Remaining() < 1e17 {
		t.Errorf("remaining = %v, want a large value when unlimited", b.Remaining())
	}
}

func TestBudgetSeed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	b := NewBudgetGuard(1.0, func() time.Time { return now }, time.UTC)
	b.Seed(0.75)
	if b.Spent() != 0.75 {
		t.Errorf("spent = %v, want 0.75", b.Spent())
	}
	if !b.Allow() {
		t.Error("should allow at 0.75/1.0")
	}
}

func TestBudgetGuardIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	b := NewBudgetGuard(1000, time.Now, time.UTC)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Add(0.01)
			b.Allow()
			b.Remaining()
		}()
	}
	wg.Wait()
	if got := b.Spent(); got < 0.499 || got > 0.501 {
		t.Errorf("spent = %v, want 0.5", got)
	}
}

func TestTallyFooter(t *testing.T) {
	t.Parallel()
	var tally Tally
	tally.Add(Response{Usage: Usage{PromptTokens: 21000, CompletionTokens: 34000, CostUSD: 0.008, CostKnown: true}, Retries: 3})
	tally.Add(Response{Usage: Usage{PromptTokens: 430, CompletionTokens: 880, CostUSD: 0.001, CostKnown: true}, Retries: 1})

	got := tally.Totals()
	if got.Calls != 2 || got.PromptTokens != 21430 || got.CompletionTokens != 34880 || got.Retries != 4 {
		t.Errorf("totals = %+v", got)
	}
	if got.UnknownCost != 0 {
		t.Errorf("UnknownCost = %d, want 0", got.UnknownCost)
	}
	const want = "2 calls · 21430 in / 34880 out · ~$0.009 · 4 retries"
	if got.String() != want {
		t.Errorf("footer =\n%q\nwant\n%q", got.String(), want)
	}
}

func TestTallyReportsUnknownCost(t *testing.T) {
	t.Parallel()
	var tally Tally
	tally.Add(Response{Usage: Usage{PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.01, CostKnown: true}})
	tally.Add(Response{Usage: Usage{PromptTokens: 100, CompletionTokens: 50}}) // unpriced model
	tally.Add(Response{Retries: 2})                                            // a hard failure: no tokens, no cost

	got := tally.Totals()
	if got.Calls != 3 || got.Retries != 2 {
		t.Errorf("totals = %+v", got)
	}
	if got.UnknownCost != 1 {
		t.Errorf("UnknownCost = %d, want 1: a failed call costs nothing, an unpriced one costs something unknown", got.UnknownCost)
	}
	if s := got.String(); !strings.Contains(s, "1 call of unknown cost") {
		t.Errorf("footer = %q, want it to admit the missing cost", s)
	}
}

func TestTallyIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	var tally Tally
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tally.Add(Response{Usage: Usage{PromptTokens: 2, CostUSD: 0.001, CostKnown: true}, Retries: 1})
		}()
	}
	wg.Wait()
	got := tally.Totals()
	if got.Calls != 50 || got.PromptTokens != 100 || got.Retries != 50 {
		t.Errorf("totals = %+v", got)
	}
}
