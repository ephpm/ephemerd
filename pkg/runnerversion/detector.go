package runnerversion

import (
	"log/slog"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
)

// Detector is mechanism 2: it recognises a runner that registered and
// exited on its own, quickly, without any job-started signal, and trips a
// per-platform circuit breaker after Threshold such runners in a row.
//
// exit_code is deliberately NOT an input: run.sh exits 0 after a refused
// connection, a completed job and a cancelled job alike.
//
// Wiring (next increment): Registered from trackRunning, JobStarted from
// handleInProgress (and a busy-poll verdict), Exited from the wait
// goroutines with killedByUs = (jobCtx.Err() != nil || orphan sweep ||
// drain), Allow from admitDispatch.
type Detector struct {
	// Window is the registered→self-exit lifetime below which a runner
	// with no job-started signal is zero-work. Default 120s.
	Window time.Duration
	// Threshold is the consecutive zero-work count per platform that opens
	// the breaker. Default 3.
	Threshold int
	// Cooldown is how long the breaker stays OPEN before allowing one
	// probe (HALF-OPEN). Default 15m.
	Cooldown time.Duration
	// OnDegraded is called once per CLOSED→OPEN transition. Alert + (when
	// enabled) upgrade hook. Called without the lock held.
	OnDegraded func(platform string, streak int, reason string)
	Log        *slog.Logger
	Now        func() time.Time

	mu       sync.Mutex
	runners  map[string]*runnerRec
	breakers map[string]*breaker
}

type runnerRec struct {
	platform     string
	registeredAt time.Time
	jobStarted   bool
}

// BreakerState is the per-platform breaker state.
type BreakerState int

const (
	BreakerClosed BreakerState = iota
	BreakerOpen
	BreakerHalfOpen
)

func (b BreakerState) String() string {
	switch b {
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

type breaker struct {
	state    BreakerState
	streak   int
	openedAt time.Time
	// probing is true while the single HALF-OPEN probe is outstanding.
	probing bool
}

// Verdict is Exited's judgement of one runner.
type Verdict struct {
	Runner   string
	Platform string
	Lifetime time.Duration
	ZeroWork bool
	// Reason: "job-started", "killed-by-us", "lived-past-window",
	// "unknown-runner", or "zero-work".
	Reason string
	// Streak is the platform's consecutive zero-work count after this exit.
	Streak int
	// Opened is true when THIS exit opened the breaker.
	Opened bool
}

const (
	DefaultZeroWorkWindow    = 120 * time.Second
	DefaultZeroWorkThreshold = 3
	DefaultBreakerCooldown   = 15 * time.Minute
)

func (d *Detector) init() {
	if d.runners == nil {
		d.runners = map[string]*runnerRec{}
	}
	if d.breakers == nil {
		d.breakers = map[string]*breaker{}
	}
	if d.Window <= 0 {
		d.Window = DefaultZeroWorkWindow
	}
	if d.Threshold <= 0 {
		d.Threshold = DefaultZeroWorkThreshold
	}
	if d.Cooldown <= 0 {
		d.Cooldown = DefaultBreakerCooldown
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

func (d *Detector) breakerLocked(platform string) *breaker {
	b, ok := d.breakers[platform]
	if !ok {
		b = &breaker{}
		d.breakers[platform] = b
	}
	return b
}

// Registered records that a runner was provisioned on a platform.
func (d *Detector) Registered(runnerName, platform string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	d.runners[runnerName] = &runnerRec{platform: platform, registeredAt: d.Now()}
}

// JobStarted records a job-started signal for a runner (in_progress webhook
// naming it, or a busy poll). It resets the platform's streak and closes
// its breaker: the platform is demonstrably dispatching to our runners.
func (d *Detector) JobStarted(runnerName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	rec, ok := d.runners[runnerName]
	if !ok {
		return
	}
	rec.jobStarted = true
	b := d.breakerLocked(rec.platform)
	if b.state != BreakerClosed || b.streak != 0 {
		d.Log.Info("runner breaker closed: job started", "platform", rec.platform, "runner", runnerName, "was", b.state.String())
	}
	b.state, b.streak, b.probing = BreakerClosed, 0, false
	metrics.RunnerBreakerOpen.WithLabelValues(rec.platform).Set(0)
}

// Exited records that a runner's process ended. killedByUs must be true
// when ephemerd ended it (timeout, drain, orphan sweep, shutdown); such
// exits are never zero-work. Returns the verdict; the caller does nothing
// with it except log — the breaker and OnDegraded are driven here.
func (d *Detector) Exited(runnerName string, killedByUs bool) Verdict {
	d.mu.Lock()
	d.init()
	rec, ok := d.runners[runnerName]
	delete(d.runners, runnerName)
	if !ok {
		d.mu.Unlock()
		return Verdict{Runner: runnerName, Reason: "unknown-runner"}
	}
	v := Verdict{Runner: runnerName, Platform: rec.platform, Lifetime: d.Now().Sub(rec.registeredAt)}
	b := d.breakerLocked(rec.platform)
	switch {
	case rec.jobStarted:
		v.Reason = "job-started"
	case killedByUs:
		v.Reason = "killed-by-us"
	case v.Lifetime >= d.Window:
		v.Reason = "lived-past-window"
	default:
		v.ZeroWork = true
		v.Reason = "zero-work"
	}
	if b.probing {
		b.probing = false
	}
	if !v.ZeroWork {
		// "lived-past-window" is not a refusal, but not evidence of
		// dispatch either (an orphan that idled out on its own); it
		// leaves the streak alone. Only a job-started signal resets it.
		v.Streak = b.streak
		d.mu.Unlock()
		return v
	}

	metrics.RunnerZeroWorkTotal.WithLabelValues(rec.platform).Inc()
	b.streak++
	v.Streak = b.streak
	var fire func()
	switch b.state {
	case BreakerHalfOpen:
		// The probe was zero-work: back to OPEN, new cooldown, no new alert.
		b.state = BreakerOpen
		b.openedAt = d.Now()
		d.Log.Warn("runner breaker re-opened: probe got no job", "platform", rec.platform, "runner", runnerName, "streak", b.streak)
	case BreakerClosed:
		if b.streak >= d.Threshold {
			b.state = BreakerOpen
			b.openedAt = d.Now()
			v.Opened = true
			metrics.RunnerBreakerOpen.WithLabelValues(rec.platform).Set(1)
			platform, streak := rec.platform, b.streak
			d.Log.Error("runner pool degraded: consecutive runners registered and exited without a job",
				"platform", platform, "streak", streak, "window", d.Window.String(),
				"hint", "runner below GitHub's minimum version, or webhook delivery is broken")
			if d.OnDegraded != nil {
				fire = func() { d.OnDegraded(platform, streak, "zero-work") }
			}
		}
	case BreakerOpen:
		// Exits that were already in flight when the breaker opened.
	}
	d.Log.Warn("zero-work runner", "platform", rec.platform, "runner", runnerName,
		"lifetime", v.Lifetime.Round(time.Second).String(), "streak", b.streak, "breaker", b.state.String())
	d.mu.Unlock()
	if fire != nil {
		fire()
	}
	return v
}

// Allow reports whether the scheduler may provision a runner for platform.
// CLOSED: yes. OPEN: no until Cooldown elapses, then HALF-OPEN. HALF-OPEN:
// exactly one probe; further calls say no until that probe exits.
func (d *Detector) Allow(platform string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	b := d.breakerLocked(platform)
	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if d.Now().Sub(b.openedAt) < d.Cooldown {
			return false
		}
		b.state = BreakerHalfOpen
		b.probing = true
		d.Log.Info("runner breaker half-open: allowing one probe", "platform", platform)
		return true
	case BreakerHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
	return true
}

// State returns the breaker state and streak for a platform.
func (d *Detector) State(platform string) (BreakerState, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	b := d.breakerLocked(platform)
	return b.state, b.streak
}

// Reset closes a platform's breaker (operator action, or after an upgrade
// so the next runner is admitted immediately rather than after Cooldown).
func (d *Detector) Reset(platform string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	b := d.breakerLocked(platform)
	b.state, b.streak, b.probing = BreakerClosed, 0, false
	metrics.RunnerBreakerOpen.WithLabelValues(platform).Set(0)
}

// Tracked is the number of runners registered and not yet exited.
func (d *Detector) Tracked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.runners)
}
