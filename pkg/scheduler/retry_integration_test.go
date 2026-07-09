package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// TestRetryQueue_Integration_ClaimFailureEnqueues verifies that when
// handleLocalJob's claim fails with a retryable error, the retry queue
// picks it up. Uses a claim provider that always errors and asserts the
// queue depth after the failure path returns.
func TestRetryQueue_Integration_ClaimFailureEnqueues(t *testing.T) {
	clk := newFakeClock(time.Now())
	// Retryable error: 500 server side.
	p := newClaimErrorProvider("github", errors.New("HTTP 500 internal server error"))

	s := New(Config{
		Providers:     []providers.Provider{p},
		MaxConcurrent: 1,
		Log:           testLogger(),
		Retry: RetryConfig{
			Enabled:  true,
			Schedule: []time.Duration{30 * time.Second},
			Jitter:   0,
			MaxAge:   1 * time.Hour,
			Now:      clk.Now,
		},
	})

	if s.retry == nil {
		t.Fatal("retry queue should be constructed when Retry.Enabled=true")
	}

	event := providers.JobEvent{
		Provider: p,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    100,
	}

	// handleLocalJob runs synchronously here because handleQueued goes
	// through unsee -> sem release -> enqueueRetryIfEligible before
	// returning. But it does spawn the wait goroutine on success; on
	// failure it returns after enqueue.
	s.handleLocalJob(context.Background(), event)

	if got := s.retry.Len(); got != 1 {
		t.Errorf("retry queue Len() = %d, want 1 after retryable claim failure", got)
	}
	// Non-retryable error should NOT enqueue.
	p2 := newClaimErrorProvider("github", errors.New("POST .../runners: 404 Not Found"))
	s2 := New(Config{
		Providers:     []providers.Provider{p2},
		MaxConcurrent: 1,
		Log:           testLogger(),
		Retry: RetryConfig{
			Enabled:  true,
			Schedule: []time.Duration{30 * time.Second},
			Jitter:   0,
			MaxAge:   1 * time.Hour,
			Now:      clk.Now,
		},
	})
	event2 := providers.JobEvent{
		Provider: p2, Action: "queued", Repo: "myrepo", JobID: 101,
	}
	s2.handleLocalJob(context.Background(), event2)
	if got := s2.retry.Len(); got != 0 {
		t.Errorf("non-retryable 404 should not enqueue, got Len() = %d", got)
	}
}

// TestRetryQueue_Integration_CompletedDropsRetry pins the interaction
// with handleCompleted: an outstanding retry for a job that then
// completes elsewhere must be dropped so we stop reattempting.
func TestRetryQueue_Integration_CompletedDropsRetry(t *testing.T) {
	clk := newFakeClock(time.Now())
	p := newClaimErrorProvider("github", errors.New("HTTP 500"))
	s := New(Config{
		Providers:     []providers.Provider{p},
		MaxConcurrent: 1,
		Log:           testLogger(),
		Retry: RetryConfig{
			Enabled:  true,
			Schedule: []time.Duration{30 * time.Second},
			Jitter:   0,
			MaxAge:   1 * time.Hour,
			Now:      clk.Now,
		},
	})

	event := providers.JobEvent{
		Provider: p, Action: "queued", Repo: "myrepo", JobID: 200,
	}
	s.handleLocalJob(context.Background(), event)
	if s.retry.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.retry.Len())
	}

	// Completed webhook arrives for the same job (peer picked it up).
	completed := providers.JobEvent{
		Provider: p, Action: "completed", Repo: "myrepo", JobID: 200,
		Conclusion: "success",
	}
	s.handleCompleted(context.Background(), completed)

	if got := s.retry.Len(); got != 0 {
		t.Errorf("Len() after completed = %d, want 0 (retry should be dropped)", got)
	}
}

// TestRetryQueue_Integration_Disabled verifies the pre-existing "log
// and drop" behavior is preserved when Retry.Enabled=false. Nothing
// enqueues, no goroutine leaks, no crash.
func TestRetryQueue_Integration_Disabled(t *testing.T) {
	p := newClaimErrorProvider("github", errors.New("HTTP 500"))
	s := New(Config{
		Providers:     []providers.Provider{p},
		MaxConcurrent: 1,
		Log:           testLogger(),
		// Retry left zero-valued  -  Enabled defaults to false.
	})

	if s.retry != nil {
		t.Errorf("retry queue should be nil when Retry.Enabled=false, got %v", s.retry)
	}

	event := providers.JobEvent{
		Provider: p, Action: "queued", Repo: "myrepo", JobID: 300,
	}
	s.handleLocalJob(context.Background(), event) // must not panic

	// enqueueRetryIfEligible is a no-op  -  no queue exists to check.
}
