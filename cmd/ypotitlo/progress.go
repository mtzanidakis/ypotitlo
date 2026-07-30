package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// spinnerFrames is the braille spinner. Every frame is one cell wide, so the
// line never reflows as it animates.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

const spinnerInterval = 100 * time.Millisecond

// Marks left behind when a phase ends. A phase during which something was
// warned about gets the second one: a tick above a warning line would be
// contradicting it.
const (
	markDone = "✓"
	markWarn = "!"
)

// progressUI draws a single self-updating status line: what the run is doing
// now, how long it has been going, and — once there is enough evidence to say —
// how much longer it will take.
//
// It exists because a translation is minutes of silence otherwise, and silence
// is indistinguishable from a hang. The stall watchdog protects the run; this
// protects the person watching it.
//
// Every method is safe to call from any goroutine: the translate package
// reports progress from its workers.
type progressUI struct {
	w       io.Writer
	enabled bool

	mu       sync.Mutex
	frame    int
	phase    string
	done     int
	total    int
	started  time.Time // whole run
	phaseAt  time.Time // current phase, for the ETA
	lastLine int       // width of what was drawn, so it can be erased
	warned   bool      // something was warned about during this phase
	stopped  bool

	stop chan struct{}
	wg   sync.WaitGroup
	now  func() time.Time
}

func newProgressUI(w io.Writer, enabled bool, now func() time.Time) *progressUI {
	if now == nil {
		now = time.Now
	}
	t := now()
	return &progressUI{
		w: w, enabled: enabled, started: t, phaseAt: t,
		stop: make(chan struct{}), now: now,
	}
}

// Start begins animating. It is a no-op when the UI is disabled, so a piped or
// redirected run stays clean and a -q run stays quiet.
func (p *progressUI) Start() {
	if !p.enabled {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(spinnerInterval)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				p.mu.Lock()
				p.frame++
				p.draw()
				p.mu.Unlock()
			}
		}
	}()
}

// Phase names what the run is doing now and restarts the ETA, since the
// previous phase's rate says nothing about this one.
func (p *progressUI) Phase(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reaching a new phase means the previous one ended, so it is left on the
	// screen with how long it took instead of being overwritten in place.
	p.finish()

	p.phase = name
	p.phaseAt = p.now()
	p.done, p.total = 0, 0
	p.warned = false
	p.draw()
}

// Complete ends the current phase successfully and stops the display. Used on
// the path where the run produced a file; Stop alone leaves no mark, which is
// what a failed run wants.
func (p *progressUI) Complete() {
	p.mu.Lock()
	p.finish()
	p.mu.Unlock()
	p.Stop()
}

// finish leaves a completed phase on its own line. Caller holds the lock.
func (p *progressUI) finish() {
	if !p.enabled || p.phase == "" {
		return
	}
	p.erase()

	mark := markDone
	if p.warned {
		mark = markWarn
	}
	line := mark + " " + p.phase
	if p.total > 0 {
		line += fmt.Sprintf(" %d/%d", p.done, p.total)
	}
	line += " · " + clock(p.now().Sub(p.phaseAt))
	_, _ = fmt.Fprintf(p.w, "%s\n", line)

	p.phase = ""
	p.lastLine = 0
}

// Progress updates the cue counter that drives the ETA.
func (p *progressUI) Progress(done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done, p.total = done, total
	p.draw()
}

// Suspend clears the status line, runs f, and lets the next tick redraw. Any
// warning printed during a run has to go through this, or it lands in the
// middle of the spinner line and both become unreadable.
//
// The lock is held across f deliberately. Releasing it first left a window in
// which the animating goroutine redrew the status line between the erase and the
// warning, producing exactly the mangled output this exists to prevent. f must
// therefore not call back into the UI; both callers only write to stderr.
func (p *progressUI) Suspend(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	f()
}

// SuspendWarn is Suspend for output that reports a problem. It also marks the
// current phase, so the line it eventually leaves behind does not claim success
// directly above a warning explaining what went wrong.
func (p *progressUI) SuspendWarn(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	p.warned = true
	f()
}

// EndPhase closes the current phase without opening another, so a caller can
// print something between two phases and have it appear in order.
func (p *progressUI) EndPhase() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finish()
}

// Stop ends the animation and clears the line. Safe to call twice.
func (p *progressUI) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()

	if p.enabled {
		close(p.stop)
		p.wg.Wait()
		p.mu.Lock()
		p.erase()
		p.mu.Unlock()
	}
}

// Elapsed is the whole run's duration, for the closing summary.
func (p *progressUI) Elapsed() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Sub(p.started)
}

// draw renders the status line. Caller holds the lock.
func (p *progressUI) draw() {
	if !p.enabled || p.stopped || p.phase == "" {
		return
	}
	var b strings.Builder
	b.WriteRune(spinnerFrames[p.frame%len(spinnerFrames)])
	b.WriteByte(' ')
	b.WriteString(p.phase)

	if p.total > 0 {
		fmt.Fprintf(&b, " %d/%d", p.done, p.total)
	}
	fmt.Fprintf(&b, " · %s", clock(p.now().Sub(p.started)))
	if eta, ok := p.eta(); ok {
		fmt.Fprintf(&b, " · eta %s", clock(eta))
	}

	line := b.String()
	// Pad to the previous width so a shrinking line does not leave debris.
	pad := p.lastLine - runeLen(line)
	if pad < 0 {
		pad = 0
	}
	_, _ = fmt.Fprintf(p.w, "\r%s%s", line, strings.Repeat(" ", pad))
	p.lastLine = runeLen(line)
}

// eta extrapolates from the current phase's rate. It stays hidden until a tenth
// of the work is done, because an estimate from two cues out of seven hundred is
// noise presented as information.
func (p *progressUI) eta() (time.Duration, bool) {
	if p.total <= 0 || p.done <= 0 || p.done*10 < p.total {
		return 0, false
	}
	if p.done >= p.total {
		return 0, false
	}
	spent := p.now().Sub(p.phaseAt)
	if spent <= 0 {
		return 0, false
	}
	perCue := spent / time.Duration(p.done)
	return perCue * time.Duration(p.total-p.done), true
}

func (p *progressUI) erase() {
	if !p.enabled || p.lastLine == 0 {
		return
	}
	_, _ = fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastLine))
	p.lastLine = 0
}

func runeLen(s string) int { return len([]rune(s)) }

// clock formats a duration as m:ss, or h:mm:ss once it runs past an hour.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Round(time.Second).Seconds())
	h, m, s := total/3600, (total/60)%60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
