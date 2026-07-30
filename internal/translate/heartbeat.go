package translate

import (
	"sync"
	"time"
)

// Heartbeat carries "something is still happening" from the HTTP client to the
// stall watchdog.
//
// The watchdog measures progress, and until now the finest thing it could see
// was a provider call returning. That is too coarse: one call retries internally
// and can legitimately run for twelve minutes against a provider answering 503,
// so a run being retried looked exactly like a run that had hung. The client
// beats once per HTTP exchange, which is the smallest event that distinguishes
// them.
//
// It is created by the caller because the caller owns both ends — it builds the
// client and it calls Run.
type Heartbeat struct {
	now func() time.Time

	mu   sync.Mutex
	last time.Time
}

// NewHeartbeat returns a Heartbeat reading the given clock, or the wall clock
// when now is nil.
func NewHeartbeat(now func() time.Time) *Heartbeat {
	if now == nil {
		now = time.Now
	}
	return &Heartbeat{now: now, last: now()}
}

// Beat records that something happened. Safe for concurrent use; it is called
// from every worker's HTTP path.
func (h *Heartbeat) Beat() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = h.now()
}

// Last is when something last happened.
func (h *Heartbeat) Last() time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}
