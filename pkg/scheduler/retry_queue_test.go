package scheduler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
	gh "github.com/google/go-github/v72/github"
)

// fakeClock is a minimal deterministic clock. The retry queue reads it
// via cfg.Now; we don't need to plug into a full clock interface because
// the queue's only time.NewTimer usage is real-time. Tests that need to
// exercise the timer directly call fireDue instead of waiting on it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func mkEvent(id int64) providers.JobEvent {
	return providers.JobEvent{
		Provider: newMockProvider("github"),
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    id,
	}
}

// TestRetryQueue_BackoffLadder pins the schedule progression. Uses a
// fixed schedule and zero jitter so the delays are exact.
func TestRetryQueue_BackoffLadder(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC))
	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second, 1 * time.Minute, 2 * time.Minute},
		Jitter:   0,
		MaxAge:   1 * time.Hour,
		Now:      clk.Now,
	}, testLogger())

	ev := mkEvent(1)
	handler := func(_ context.Context, _ providers.JobEvent) error {
		return errors.New("HTTP 500 server error")
	}

	// Attempt 1 (first failure): should schedule 30s out.
	q.Add(ev, handler, errors.New("HTTP 500 server error"))
	it := q.index[keyFor(ev)]
	if it == nil {
		t.Fatal("expected item in index after Add")
	}
	if got := it.nextAttempt.Sub(clk.Now()); got != 30*time.Second {
		t.Errorf("attempt 1 delay = %v, want 30s", got)
	}
	if it.attempts != 1 {
		t.Errorf("attempts = %d, want 1", it.attempts)
	}

	// Simulate the first retry firing and failing again.
	q.Add(ev, handler, errors.New("HTTP 500 server error"))
	if got := it.nextAttempt.Sub(clk.Now()); got != 1*time.Minute {
		t.Errorf("attempt 2 delay = %v, want 1m", got)
	}

	// Third failure.
	q.Add(ev, handler, errors.New("HTTP 500 server error"))
	if got := it.nextAttempt.Sub(clk.Now()); got != 2*time.Minute {
		t.Errorf("attempt 3 delay = %v, want 2m", got)
	}

	// Fourth failure: beyond schedule length  -  clamp to last entry.
	q.Add(ev, handler, errors.New("HTTP 500 server error"))
	if got := it.nextAttempt.Sub(clk.Now()); got != 2*time.Minute {
		t.Errorf("attempt 4 (past ladder) delay = %v, want 2m (clamped)", got)
	}
}

// TestRetryQueue_GiveUpOnMaxAge verifies the queue drops an item once
// the age since first failure exceeds MaxAge.
func TestRetryQueue_GiveUpOnMaxAge(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC))
	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second},
		Jitter:   0,
		MaxAge:   5 * time.Minute,
		Now:      clk.Now,
	}, testLogger())

	ev := mkEvent(2)
	handler := func(_ context.Context, _ providers.JobEvent) error {
		return errors.New("HTTP 500")
	}

	q.Add(ev, handler, errors.New("HTTP 500"))
	if _, ok := q.index[keyFor(ev)]; !ok {
		t.Fatal("expected item after first Add")
	}

	// Advance past MaxAge and Add again  -  should drop.
	clk.Advance(6 * time.Minute)
	q.Add(ev, handler, errors.New("HTTP 500"))
	if _, ok := q.index[keyFor(ev)]; ok {
		t.Error("expected item dropped after MaxAge exceeded, still present")
	}
	if q.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after give-up", q.Len())
	}
}

// TestRetryQueue_DropOnCompletion verifies Drop removes an outstanding
// retry  -  the completed-webhook path.
func TestRetryQueue_DropOnCompletion(t *testing.T) {
	clk := newFakeClock(time.Now())
	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second},
		Jitter:   0,
		MaxAge:   1 * time.Hour,
		Now:      clk.Now,
	}, testLogger())

	ev := mkEvent(3)
	handler := func(_ context.Context, _ providers.JobEvent) error { return errors.New("500") }
	q.Add(ev, handler, errors.New("500"))

	if q.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", q.Len())
	}

	q.Drop(keyFor(ev))

	if q.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after Drop", q.Len())
	}
	if _, ok := q.index[keyFor(ev)]; ok {
		t.Error("item still in index after Drop")
	}
}

// TestRetryQueue_NonRetryableErrorNotEnqueued pins that a 404 does not
// enter the queue.
func TestRetryQueue_NonRetryableErrorNotEnqueued(t *testing.T) {
	clk := newFakeClock(time.Now())
	q := newRetryQueue(RetryConfig{
		Enabled: true,
		Now:     clk.Now,
	}, testLogger())

	ev := mkEvent(4)
	handler := func(_ context.Context, _ providers.JobEvent) error { return nil }

	// 404 via string-match fallback.
	q.Add(ev, handler, errors.New("GET .../jobs/999: 404 Not Found"))
	if q.Len() != 0 {
		t.Errorf("404 should not enqueue, Len() = %d", q.Len())
	}

	// 422 validation.
	q.Add(ev, handler, errors.New("POST .../runners: 422 Unprocessable Entity"))
	if q.Len() != 0 {
		t.Errorf("422 should not enqueue, Len() = %d", q.Len())
	}
}

// TestRetryQueue_RateAwareSchedulesAfterReset verifies that when the
// rate hint says remaining=0 with a known reset, the next attempt is
// pushed to just after reset instead of the normal ladder delay.
func TestRetryQueue_RateAwareSchedulesAfterReset(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	reset := base.Add(45 * time.Minute) // fresh, in the future
	updated := base.Add(-30 * time.Second)

	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second}, // ladder would say 30s
		Jitter:   0,
		MaxAge:   2 * time.Hour,
		Now:      clk.Now,
		RateHint: func() (int64, time.Time, time.Time) {
			return 0, reset, updated
		},
	}, testLogger())

	ev := mkEvent(5)
	handler := func(_ context.Context, _ providers.JobEvent) error { return nil }

	// Feed a RateLimitError so class == errRateLimit.
	rle := &gh.RateLimitError{
		Rate:     gh.Rate{Remaining: 0},
		Response: &http.Response{StatusCode: http.StatusForbidden, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		Message:  "rate limit exceeded",
	}
	q.Add(ev, handler, rle)

	it := q.index[keyFor(ev)]
	if it == nil {
		t.Fatal("expected item after Add")
	}
	delay := it.nextAttempt.Sub(clk.Now())
	// Should be ~45m + 5s + [0, 20s) jitter. Definitely much bigger than
	// the 30s ladder value.
	minWant := 45*time.Minute + 5*time.Second
	maxWant := 45*time.Minute + 5*time.Second + 20*time.Second
	if delay < minWant || delay >= maxWant {
		t.Errorf("rate-aware delay = %v, want in [%v, %v)", delay, minWant, maxWant)
	}
}

// TestRetryQueue_RateAware_StaleHintIgnored verifies we DON'T snap to
// reset if the rate snapshot is older than 5 minutes  -  the reset time
// may itself be stale.
func TestRetryQueue_RateAware_StaleHintIgnored(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	reset := base.Add(45 * time.Minute)
	updated := base.Add(-10 * time.Minute) // stale

	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second},
		Jitter:   0,
		MaxAge:   2 * time.Hour,
		Now:      clk.Now,
		RateHint: func() (int64, time.Time, time.Time) {
			return 0, reset, updated
		},
	}, testLogger())

	ev := mkEvent(6)
	handler := func(_ context.Context, _ providers.JobEvent) error { return nil }
	q.Add(ev, handler, &gh.RateLimitError{
		Response: &http.Response{StatusCode: http.StatusForbidden, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
	})

	it := q.index[keyFor(ev)]
	if got := it.nextAttempt.Sub(clk.Now()); got != 30*time.Second {
		t.Errorf("stale rate hint should fall through to ladder, got delay %v want 30s", got)
	}
}

// TestClassifyErr is a table-driven check of the classification logic.
func TestClassifyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want errClass
	}{
		{"nil", nil, errNonRetryable},
		{"rate_limit_typed", &gh.RateLimitError{
			Response: &http.Response{StatusCode: 403, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errRateLimit},
		{"abuse_typed", &gh.AbuseRateLimitError{
			Response: &http.Response{StatusCode: 403, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errRateLimit},
		{"404_typed", &gh.ErrorResponse{
			Response: &http.Response{StatusCode: 404, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errNonRetryable},
		{"500_typed", &gh.ErrorResponse{
			Response: &http.Response{StatusCode: 500, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errServerSide},
		{"429_typed", &gh.ErrorResponse{
			Response: &http.Response{StatusCode: 429, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errRateLimit},
		{"422_typed", &gh.ErrorResponse{
			Response: &http.Response{StatusCode: 422, Request: &http.Request{URL: &url.URL{Path: "/x"}}},
		}, errNonRetryable},
		{"net_timeout", &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}, errNetwork},
		{"str_500", errors.New("wrapped: 500 internal server error"), errServerSide},
		{"str_502", errors.New("bad gateway 502"), errServerSide},
		{"str_rate", errors.New("secondary rate limit exceeded"), errRateLimit},
		{"str_404", errors.New("registering JIT runner: 404 Not Found"), errNonRetryable},
		{"str_permission", errors.New("permission denied"), errNonRetryable},
		{"str_timeout", errors.New("context deadline exceeded (Client.Timeout)"), errNetwork},
		{"str_unknown", errors.New("something weird happened"), errUnknownRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Errorf("classifyErr(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryQueue_FireDueRunsHandler drives the drain path directly.
// We add an item whose nextAttempt is already in the past (attempt=0
// via a manual poke) and call fireDue.
func TestRetryQueue_FireDueRunsHandler(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC))
	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second},
		Jitter:   0,
		MaxAge:   1 * time.Hour,
		Now:      clk.Now,
	}, testLogger())

	ev := mkEvent(7)
	var calls atomic.Int32
	done := make(chan struct{})
	handler := func(_ context.Context, _ providers.JobEvent) error {
		if calls.Add(1) == 1 {
			close(done)
			return nil // success first fire  -  no re-enqueue
		}
		return nil
	}

	q.Add(ev, handler, errors.New("HTTP 500"))
	// Advance the clock past the scheduled attempt so fireDue picks it up.
	clk.Advance(1 * time.Minute)
	q.fireDue(context.Background())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never fired within 2s")
	}
	// Wait a moment for the goroutine to complete so we can safely
	// inspect Len(). runOne exits immediately on nil err.
	time.Sleep(50 * time.Millisecond)
	if q.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after successful handler", q.Len())
	}
}

// TestRetryQueue_LadderAdvancesThroughFirePath is the regression test for
// the bug where fireDue deleted the popped item from q.index, so the
// re-Add in runOne always hit the `!existed` branch and rebuilt a FRESH
// item (attempts reset to 1, firstFailure reset to now). The observable
// symptoms were: the backoff ladder never advanced past schedule[0]
// (~30s forever) and the MaxAge give-up never triggered.
//
// This test drives the REAL fire path (fireDue -> runOne -> re-Add) with
// an always-failing handler and asserts:
//  1. The re-schedule delay grows 30s -> 1m -> 2m across successive fires.
//  2. attempts increments monotonically and firstFailure is preserved.
//  3. The item is dropped once its age exceeds MaxAge.
//
// Against the un-fixed code the delay stays pinned at 30s and the item
// never gives up, so both the ladder and give-up assertions fail.
func TestRetryQueue_LadderAdvancesThroughFirePath(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)
	// MaxAge chosen so the ladder (30s,1m,2m clamped) runs several rungs
	// before we deliberately blow past it at the end.
	q := newRetryQueue(RetryConfig{
		Enabled:  true,
		Schedule: []time.Duration{30 * time.Second, 1 * time.Minute, 2 * time.Minute},
		Jitter:   0,
		MaxAge:   10 * time.Minute,
		Now:      clk.Now,
	}, testLogger())

	ev := mkEvent(42)

	var calls atomic.Int32
	handler := func(_ context.Context, _ providers.JobEvent) error {
		calls.Add(1)
		return errors.New("HTTP 500 server error")
	}

	// Seed the queue as the claim path would: first failure -> attempt 1,
	// scheduled 30s out.
	q.Add(ev, handler, errors.New("HTTP 500 server error"))
	key := keyFor(ev)
	it := q.index[key]
	if it == nil {
		t.Fatal("expected item in index after seed Add")
	}
	if it.attempts != 1 {
		t.Fatalf("attempts after seed = %d, want 1", it.attempts)
	}
	if got := it.nextAttempt.Sub(clk.Now()); got != 30*time.Second {
		t.Fatalf("seed delay = %v, want 30s", got)
	}
	firstFailure := it.firstFailure

	// fireAndWait drives fireDue and blocks until the goroutine it spawns
	// (runOne) has finished its re-Add, detected by attempts reaching the
	// expected value. This avoids racing the fire goroutine.
	fireAndWait := func(expectAttempts int) {
		q.fireDue(context.Background())
		deadline := time.After(2 * time.Second)
		for {
			q.mu.Lock()
			cur, ok := q.index[key]
			var attempts int
			if ok {
				attempts = cur.attempts
			}
			q.mu.Unlock()
			if ok && attempts == expectAttempts {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for attempts=%d (have ok=%v attempts=%d)", expectAttempts, ok, attempts)
			case <-time.After(2 * time.Millisecond):
			}
		}
	}

	// Fire 1: 30s scheduled -> advance to it, fire. runOne fails and
	// re-Adds: attempts 1->2, next delay should be 1m (ladder[1]).
	clk.Advance(30 * time.Second)
	fireAndWait(2)
	it = q.index[key]
	if got := it.nextAttempt.Sub(clk.Now()); got != 1*time.Minute {
		t.Errorf("after fire 1: delay = %v, want 1m (ladder must advance, not reset to 30s)", got)
	}
	if !it.firstFailure.Equal(firstFailure) {
		t.Errorf("after fire 1: firstFailure changed %v -> %v (must be preserved)", firstFailure, it.firstFailure)
	}

	// Fire 2: advance the 1m, fire. attempts 2->3, next delay 2m (ladder[2]).
	clk.Advance(1 * time.Minute)
	fireAndWait(3)
	it = q.index[key]
	if got := it.nextAttempt.Sub(clk.Now()); got != 2*time.Minute {
		t.Errorf("after fire 2: delay = %v, want 2m", got)
	}

	// Fire 3: advance the 2m, fire. attempts 3->4, past ladder end so
	// clamps to last rung (2m). Still under MaxAge (total ~3.5m).
	clk.Advance(2 * time.Minute)
	fireAndWait(4)
	it = q.index[key]
	if got := it.nextAttempt.Sub(clk.Now()); got != 2*time.Minute {
		t.Errorf("after fire 3: delay = %v, want 2m (clamped)", got)
	}

	// Now blow past MaxAge: jump the clock well beyond the 10m budget
	// from firstFailure, then fire once more. runOne fails, re-Adds, and
	// Add's give-up check fires -> item removed from the queue entirely.
	clk.Advance(20 * time.Minute)
	q.fireDue(context.Background())
	deadline := time.After(2 * time.Second)
	for {
		if q.Len() == 0 {
			q.mu.Lock()
			_, stillIndexed := q.index[key]
			q.mu.Unlock()
			if !stillIndexed {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for MaxAge give-up; Len=%d", q.Len())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestRetryQueue_Disabled_IsNoOp verifies that when Enabled=false,
// Add is a no-op  -  the pre-existing "log and drop" behavior is preserved.
func TestRetryQueue_Disabled_IsNoOp(t *testing.T) {
	q := newRetryQueue(RetryConfig{Enabled: false}, testLogger())
	ev := mkEvent(8)
	q.Add(ev, func(_ context.Context, _ providers.JobEvent) error { return nil },
		errors.New("HTTP 500"))
	if q.Len() != 0 {
		t.Errorf("disabled queue Len() = %d, want 0", q.Len())
	}
	if q.Enabled() {
		t.Error("Enabled() = true, want false")
	}
}
