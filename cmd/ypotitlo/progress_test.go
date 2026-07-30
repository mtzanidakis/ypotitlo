package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// A disabled UI must write nothing at all: a piped or -q run has to stay clean,
// and carriage returns in a log file are worse than no progress.
func TestProgressUIDisabledWritesNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, false, nil)
	p.Start()
	p.Phase("brief")
	p.Progress(10, 100)
	p.Stop()

	if buf.Len() != 0 {
		t.Errorf("disabled UI wrote %q", buf.String())
	}
}

func TestProgressUIShowsPhaseAndCounts(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	now := time.Unix(0, 0).UTC()
	clockFn := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, clockFn)

	p.Phase("brief")
	if got := buf.String(); !strings.Contains(got, "brief") {
		t.Errorf("output = %q, want the phase name", got)
	}

	mu.Lock()
	now = now.Add(90 * time.Second)
	mu.Unlock()

	buf.Reset()
	p.Phase("translating")
	p.Progress(200, 700)
	got := buf.String()
	for _, want := range []string{"translating", "200/700", "1:30"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to contain %q", got, want)
		}
	}
}

// The ETA appears only once there is enough work behind it, and then counts
// down between updates instead of being recomputed from a stale count.
//
// Recomputing it every frame was visibly wrong: progress arrives in batches, so
// between two of them the elapsed time grows while the completed count does not,
// and elapsed÷done×remaining therefore climbed by a second every second before
// dropping back when the next batch landed.
func TestETACountsDownBetweenUpdates(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	now := time.Unix(0, 0).UTC()
	clockFn := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, clockFn)
	p.Phase("translating")

	advance(10 * time.Second)
	buf.Reset()
	p.Progress(2, 700) // under the threshold: no estimate yet
	if strings.Contains(buf.String(), "eta") {
		t.Errorf("an ETA was shown from 2/700: %q", buf.String())
	}

	// Half done after 10s -> about 10s remaining.
	buf.Reset()
	p.Progress(350, 700)
	first, ok := p.etaFor(t)
	if !ok {
		t.Fatalf("no ETA at 350/700: %q", buf.String())
	}

	// Time passes with no new progress. The estimate must fall, not climb.
	advance(4 * time.Second)
	second, ok := p.etaFor(t)
	if !ok {
		t.Fatal("the ETA disappeared while waiting")
	}
	if second >= first {
		t.Errorf("ETA went from %v to %v with no progress; it must count down", first, second)
	}
	if want := first - 4*time.Second; second != want {
		t.Errorf("ETA = %v after 4s, want %v", second, want)
	}
}

// It stops being shown once it runs out, however long a final batch takes.
func TestETAIsWithdrawnWhenExhausted(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	now := time.Unix(0, 0).UTC()
	clockFn := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, clockFn)
	p.Phase("translating")

	mu.Lock()
	now = now.Add(10 * time.Second)
	mu.Unlock()
	p.Progress(350, 700)

	mu.Lock()
	now = now.Add(time.Hour)
	mu.Unlock()

	// An exhausted estimate is withdrawn rather than pinned at zero: it is no
	// longer an estimate, and "eta 0:00" for minutes on end reads as a broken
	// display rather than as "no longer known".
	if _, ok := p.etaFor(t); ok {
		t.Error("an ETA was still shown after the estimate ran out")
	}
}

// Suspend has to clear the line before a warning is printed, or the two collide.
func TestProgressUISuspendClearsTheLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, func() time.Time { return time.Unix(0, 0).UTC() })
	p.Phase("translating")
	p.Progress(1, 10)

	buf.Reset()
	p.Suspend(func() { _, _ = buf.WriteString("warning: something\n") })

	got := buf.String()
	if !strings.HasPrefix(got, "\r") {
		t.Errorf("output = %q, want it to start by returning to column zero", got)
	}
	if !strings.Contains(got, "warning: something") {
		t.Errorf("output = %q, want the warning", got)
	}
	// The spinner text must not survive ahead of the warning.
	if strings.Contains(strings.Split(got, "warning:")[0], "translating") {
		t.Errorf("the status line was not cleared before the warning: %q", got)
	}
}

func TestClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0:00"},
		{9 * time.Second, "0:09"},
		{90 * time.Second, "1:30"},
		{14*time.Minute + 12*time.Second, "14:12"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
		{-5 * time.Second, "0:00"},
	}
	for _, tt := range tests {
		if got := clock(tt.in); got != tt.want {
			t.Errorf("clock(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Stop must be safe twice: it is deferred and also called on the success path.
func TestProgressUIStopIsIdempotent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, nil)
	p.Start()
	p.Phase("brief")
	p.Stop()
	p.Stop()
}

// A finished phase stays on screen with how long it took, instead of being
// overwritten by the next one. Previously "brief" and "translating" shared a
// single line, so the run showed no history of what it had already done.
func TestFinishedPhasesRemainOnScreen(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { return now }
	p := newProgressUI(&buf, true, clock)

	p.Phase("brief")
	now = now.Add(65 * time.Second)
	p.Phase("translating")
	p.Progress(734, 734)
	now = now.Add(600 * time.Second)
	p.Complete()

	got := buf.String()
	for _, want := range []string{"✓ brief · 1:05", "✓ translating 734/734 · 10:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%q", want, got)
		}
	}
	// Each completed phase gets its own line.
	if n := strings.Count(got, "✓"); n != 2 {
		t.Errorf("found %d completed phases, want 2", n)
	}
}

// A phase during which something was warned about must not claim success:
// a tick directly above a warning contradicts it.
func TestAWarnedPhaseIsNotMarkedDone(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, func() time.Time { return time.Unix(0, 0).UTC() })

	p.Phase("brief")
	p.SuspendWarn(func() { _, _ = buf.WriteString("warning: brief: unavailable\n") })
	p.Phase("translating")

	got := buf.String()
	if !strings.Contains(got, markWarn+" brief") {
		t.Errorf("a warned phase should carry %q:\n%q", markWarn, got)
	}
	if strings.Contains(got, markDone+" brief") {
		t.Errorf("a warned phase must not be marked done:\n%q", got)
	}
}

// Informational output between phases must not mark the phase as warned.
func TestPlainSuspendDoesNotMarkThePhase(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, func() time.Time { return time.Unix(0, 0).UTC() })

	p.Phase("detecting language")
	p.Suspend(func() { _, _ = buf.WriteString("translating English -> Greek\n") })
	p.EndPhase()

	if !strings.Contains(buf.String(), markDone+" detecting language") {
		t.Errorf("an informational line should leave the phase marked done:\n%q", buf.String())
	}
}

// A failed run leaves no completion mark: Stop tears down without claiming the
// phase finished.
func TestStopLeavesNoCompletionMark(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := newProgressUI(&buf, true, func() time.Time { return time.Unix(0, 0).UTC() })
	p.Phase("translating")
	p.Progress(40, 734)
	p.Stop()

	if strings.Contains(buf.String(), markDone) {
		t.Errorf("a stopped run must not report a completed phase:\n%q", buf.String())
	}
}

// etaFor reads the current estimate under the lock, for tests.
func (p *progressUI) etaFor(t *testing.T) (time.Duration, bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.eta()
}

// The estimate appears at the configured share of the work and not before.
func TestETAThresholdBoundary(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0).UTC()
	p := newProgressUI(&bytes.Buffer{}, true, func() time.Time { return now })
	p.Phase("translating")
	now = now.Add(time.Minute)

	const total = 1000
	under := total*etaMinPercent/100 - 1
	p.Progress(under, total)
	if _, ok := p.etaFor(t); ok {
		t.Errorf("an ETA was shown at %d/%d, below the %d%% threshold", under, total, etaMinPercent)
	}

	at := total * etaMinPercent / 100
	p.Progress(at, total)
	if _, ok := p.etaFor(t); !ok {
		t.Errorf("no ETA at %d/%d, the %d%% threshold", at, total, etaMinPercent)
	}
}
