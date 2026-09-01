package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tinyBackoffs keeps the in-place retry tests fast while still exercising
// the sleep-between-attempts path.
var tinyBackoffs = []time.Duration{time.Millisecond, time.Millisecond}

// TestRetryEnvCreate pins the in-place create retry that rides out
// transient runtime failures (the production case: a dead containerd shim
// failing Create with "ttrpc: closed") without giving up the held slot
// and JIT claim. The short-circuits matter as much as the retries: a
// permanent error or a draining node must not burn backoff time.
func TestRetryEnvCreate(t *testing.T) {
	transient := errors.New("failed to create shim task: ttrpc: closed")
	permanent := errors.New("image does not exist")

	alwaysRetryable := func(error) bool { return true }
	notDraining := func() bool { return false }

	t.Run("first attempt succeeds: no retries", func(t *testing.T) {
		calls := 0
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			alwaysRetryable, notDraining,
			func() error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want nil, 1", err, calls)
		}
	})

	t.Run("transient failure heals on a later attempt", func(t *testing.T) {
		calls := 0
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			alwaysRetryable, notDraining,
			func() error {
				calls++
				if calls < 2 {
					return transient
				}
				return nil
			})
		if err != nil || calls != 2 {
			t.Fatalf("err = %v, calls = %d; want nil, 2", err, calls)
		}
	})

	t.Run("ladder exhausted: last error returned", func(t *testing.T) {
		calls := 0
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			alwaysRetryable, notDraining,
			func() error { calls++; return transient })
		if !errors.Is(err, transient) {
			t.Fatalf("err = %v, want the create error", err)
		}
		if want := 1 + len(tinyBackoffs); calls != want {
			t.Fatalf("calls = %d, want %d (initial + ladder)", calls, want)
		}
	})

	t.Run("permanent error short-circuits", func(t *testing.T) {
		calls := 0
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			func(error) bool { return false }, notDraining,
			func() error { calls++; return permanent })
		if !errors.Is(err, permanent) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want permanent error after 1 call", err, calls)
		}
	})

	t.Run("draining node does not retry", func(t *testing.T) {
		calls := 0
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			alwaysRetryable, func() bool { return true },
			func() error { calls++; return transient })
		if !errors.Is(err, transient) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want error after 1 call during drain", err, calls)
		}
	})

	t.Run("drain starting mid-backoff stops the next attempt", func(t *testing.T) {
		calls := 0
		draining := false
		err := retryEnvCreate(context.Background(), testLogger(), tinyBackoffs,
			alwaysRetryable, func() bool { return draining },
			func() error {
				calls++
				draining = true // drain begins after the first failure
				return transient
			})
		if !errors.Is(err, transient) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want error after 1 call", err, calls)
		}
	})

	t.Run("ctx cancellation aborts the backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		start := time.Now()
		err := retryEnvCreate(ctx, testLogger(), []time.Duration{time.Hour},
			alwaysRetryable, notDraining,
			func() error { calls++; return transient })
		if !errors.Is(err, transient) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want error after 1 call", err, calls)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v; cancelled ctx must not sit out the backoff", elapsed)
		}
	})
}

// TestClassifyErr_DeadShimIsRetryable pins the classification the create
// retry paths (both in-place and the retry-queue enqueue) depend on: the
// exact error a dead shim produced in production must NOT classify as
// non-retryable, or the job is dropped forever (observed: 1h43m of a job
// sitting queued because nothing retried a single failed create).
func TestClassifyErr_DeadShimIsRetryable(t *testing.T) {
	err := errors.New(`creating task for abc123: failed to create shim task: ttrpc: closed`)
	if class := classifyErr(err); class == errNonRetryable {
		t.Fatalf("classifyErr(dead shim create error) = %v, want retryable", class)
	}
}
