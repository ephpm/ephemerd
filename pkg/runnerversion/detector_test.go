package runnerversion

import (
	"sync"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newDetector(t *testing.T) (*Detector, *fakeClock, *[]string) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	var fired []string
	d := &Detector{
		Window:    120 * time.Second,
		Threshold: 3,
		Cooldown:  15 * time.Minute,
		Now:       clk.now,
		Log:       testLog(),
		OnDegraded: func(platform string, streak int, reason string) {
			fired = append(fired, platform)
		},
	}
	return d, clk, &fired
}

// A unique platform label per test keeps the global counter deltas honest.
func zeroWorkCount(platform string) float64 {
	return testutil.ToFloat64(metrics.RunnerZeroWorkTotal.WithLabelValues(platform))
}

func TestDetectorVerdicts(t *testing.T) {
	d, clk, _ := newDetector(t)
	const p = "verdicts-linux"
	before := zeroWorkCount(p)

	// Registered, exited in 5s on its own, no job: zero-work.
	d.Registered("r1", p)
	clk.advance(5 * time.Second)
	v := d.Exited("r1", false)
	if !v.ZeroWork || v.Reason != "zero-work" || v.Streak != 1 || v.Lifetime != 5*time.Second {
		t.Fatalf("r1: %+v", v)
	}

	// Same shape but a job started: not zero-work, and it resets the streak.
	d.Registered("r2", p)
	d.JobStarted("r2")
	clk.advance(5 * time.Second)
	v = d.Exited("r2", false)
	if v.ZeroWork || v.Reason != "job-started" || v.Streak != 0 {
		t.Fatalf("r2: %+v", v)
	}

	// Killed by us (timeout/drain/orphan sweep): never zero-work.
	d.Registered("r3", p)
	clk.advance(2 * time.Second)
	v = d.Exited("r3", true)
	if v.ZeroWork || v.Reason != "killed-by-us" {
		t.Fatalf("r3: %+v", v)
	}

	// Lived past the window with no job signal: not counted.
	d.Registered("r4", p)
	clk.advance(121 * time.Second)
	v = d.Exited("r4", false)
	if v.ZeroWork || v.Reason != "lived-past-window" {
		t.Fatalf("r4: %+v", v)
	}

	// Never registered.
	v = d.Exited("ghost", false)
	if v.ZeroWork || v.Reason != "unknown-runner" {
		t.Fatalf("ghost: %+v", v)
	}
	if d.Tracked() != 0 {
		t.Fatalf("tracked %d", d.Tracked())
	}
	if got := zeroWorkCount(p) - before; got != 1 {
		t.Fatalf("zero_work_total delta %v, want 1", got)
	}
}

func TestDetectorBreakerOpensAtThresholdOnce(t *testing.T) {
	d, clk, fired := newDetector(t)
	const p = "breaker-linux"
	const other = "breaker-windows"

	for i, name := range []string{"a", "b"} {
		d.Registered(name, p)
		clk.advance(time.Second)
		v := d.Exited(name, false)
		if v.Opened || !d.Allow(p) {
			t.Fatalf("exit %d opened early: %+v", i, v)
		}
	}
	d.Registered("c", p)
	clk.advance(time.Second)
	v := d.Exited("c", false)
	if !v.Opened || v.Streak != 3 {
		t.Fatalf("third exit should open: %+v", v)
	}
	if st, _ := d.State(p); st != BreakerOpen {
		t.Fatalf("state %s", st)
	}
	if d.Allow(p) {
		t.Fatal("open breaker must refuse")
	}
	if !d.Allow(other) {
		t.Fatal("other platform unaffected")
	}
	if testutil.ToFloat64(metrics.RunnerBreakerOpen.WithLabelValues(p)) != 1 {
		t.Fatal("breaker gauge should be 1")
	}

	// Exits already in flight do not re-fire the alert.
	d.Registered("d", p)
	clk.advance(time.Second)
	v = d.Exited("d", false)
	if v.Opened || v.Streak != 4 {
		t.Fatalf("in-flight exit: %+v", v)
	}
	if len(*fired) != 1 || (*fired)[0] != p {
		t.Fatalf("OnDegraded fired %v, want once for %s", *fired, p)
	}
}

func TestDetectorHalfOpenProbe(t *testing.T) {
	d, clk, fired := newDetector(t)
	const p = "probe-linux"
	for _, name := range []string{"a", "b", "c"} {
		d.Registered(name, p)
		clk.advance(time.Second)
		d.Exited(name, false)
	}
	if d.Allow(p) {
		t.Fatal("should be open")
	}
	clk.advance(14 * time.Minute)
	if d.Allow(p) {
		t.Fatal("still cooling down")
	}
	clk.advance(2 * time.Minute)
	if !d.Allow(p) {
		t.Fatal("cooldown elapsed: one probe allowed")
	}
	if st, _ := d.State(p); st != BreakerHalfOpen {
		t.Fatalf("state %s", st)
	}
	if d.Allow(p) {
		t.Fatal("only ONE probe while half-open")
	}

	// Probe is zero-work: back to OPEN, fresh cooldown, no second alert.
	d.Registered("probe1", p)
	clk.advance(time.Second)
	v := d.Exited("probe1", false)
	if !v.ZeroWork || v.Opened {
		t.Fatalf("probe verdict %+v", v)
	}
	if st, _ := d.State(p); st != BreakerOpen || d.Allow(p) {
		t.Fatalf("probe failure should reopen: %s", st)
	}
	if len(*fired) != 1 {
		t.Fatalf("alert fired %d times, want 1", len(*fired))
	}

	// Next probe gets a job: breaker closes, streak resets, gauge 0.
	clk.advance(16 * time.Minute)
	if !d.Allow(p) {
		t.Fatal("second probe")
	}
	d.Registered("probe2", p)
	d.JobStarted("probe2")
	if st, streak := d.State(p); st != BreakerClosed || streak != 0 {
		t.Fatalf("after job started: %s/%d", st, streak)
	}
	if !d.Allow(p) {
		t.Fatal("closed breaker allows")
	}
	if testutil.ToFloat64(metrics.RunnerBreakerOpen.WithLabelValues(p)) != 0 {
		t.Fatal("breaker gauge should be 0")
	}
}

func TestDetectorResetAndDefaults(t *testing.T) {
	d := &Detector{Log: testLog()}
	const p = "defaults-linux"
	d.Registered("x", p)
	d.Exited("x", false) // zero lifetime < default window
	if d.Window != DefaultZeroWorkWindow || d.Threshold != DefaultZeroWorkThreshold || d.Cooldown != DefaultBreakerCooldown {
		t.Fatal("defaults not applied")
	}
	if _, streak := d.State(p); streak != 1 {
		t.Fatalf("streak %d", streak)
	}
	d.Reset(p)
	if st, streak := d.State(p); st != BreakerClosed || streak != 0 {
		t.Fatal("reset")
	}
}
