package llm

import (
	"sync"
	"time"
)

// BudgetGuard enforces a daily USD budget. It tracks spend for the current UTC
// date and refuses further calls once the limit is reached. Safe for concurrent
// use.
//
// It is the only brake between a retry storm and an unbounded bill, so it is
// kept exactly as ported.
type BudgetGuard struct {
	mu    sync.Mutex
	limit float64
	spent float64
	day   string
	now   func() time.Time
	loc   *time.Location
}

// NewBudgetGuard builds a guard with the given daily USD limit. now/loc default
// to time.Now / UTC.
func NewBudgetGuard(limit float64, now func() time.Time, loc *time.Location) *BudgetGuard {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.UTC
	}
	return &BudgetGuard{limit: limit, now: now, loc: loc, day: now().In(loc).Format("2006-01-02")}
}

func (b *BudgetGuard) rollover() {
	today := b.now().In(b.loc).Format("2006-01-02")
	if today != b.day {
		b.day = today
		b.spent = 0
	}
}

// Seed sets the amount already spent today, for callers that carry spend across
// runs.
func (b *BudgetGuard) Seed(spent float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	b.spent = spent
}

// Allow reports whether another call may proceed (spend below the limit). A
// non-positive limit means unlimited.
func (b *BudgetGuard) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	if b.limit <= 0 {
		return true
	}
	return b.spent < b.limit
}

// Add records actual spend from a completed call.
func (b *BudgetGuard) Add(costUSD float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	b.spent += costUSD
}

// Spent returns today's spend.
func (b *BudgetGuard) Spent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	return b.spent
}

// Remaining returns the budget headroom (a large value when unlimited).
func (b *BudgetGuard) Remaining() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	if b.limit <= 0 {
		return 1e18
	}
	return b.limit - b.spent
}
