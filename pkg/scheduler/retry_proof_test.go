package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// TestRetryQueue_StillFailingRetryStaysEnqueued proves that when a retry
// FIRES and the claim fails again (the real production path via
// retryHandler->handleQueued->enqueueRetryIfEligible), the job stays in
// the queue with an advanced attempt count instead of being dropped.
func TestRetryQueue_StillFailingRetryStaysEnqueued(t *testing.T) {
	clk := newFakeClock(time.Now())
	p := newClaimErrorProvider("github", errors.New("HTTP 500 internal server error"))
	s := New(Config{
		Providers: []providers.Provider{p}, MaxConcurrent: 1, Log: testLogger(),
		Retry: RetryConfig{Enabled: true, Schedule: []time.Duration{30 * time.Second, 1 * time.Minute},
			Jitter: 0, MaxAge: 1 * time.Hour, Now: clk.Now},
	})
	ev := providers.JobEvent{Provider: p, Action: "queued", Repo: "myrepo", JobID: 100}

	s.handleLocalJob(context.Background(), ev) // seed: claim #1 fails -> enqueue
	if got := s.retry.Len(); got != 1 {
		t.Fatalf("seed: Len=%d want 1", got)
	}

	clk.Advance(30 * time.Second)
	s.retry.fireDue(context.Background()) // fires runOne -> retryHandler -> claim #2 fails

	// wait until the re-dispatched claim actually happened
	deadline := time.Now().Add(2 * time.Second)
	for p.claims.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let runOne finish

	if got := p.claims.Load(); got < 2 {
		t.Fatalf("retry never re-attempted the claim (claims=%d)", got)
	}
	if got := s.retry.Len(); got != 1 {
		t.Fatalf("BUG: after a still-failing retry, Len=%d want 1 (job was dropped instead of re-scheduled)", got)
	}
}
