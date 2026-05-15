package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWithPullLock_SerializesConcurrentCallers asserts that two goroutines
// holding the pull lock cannot run their critical section simultaneously
// — exactly the contract PullImage relies on to keep the content store
// happy. We feed in fake "pull" funcs and watch the in-flight counter to
// confirm only one runs at a time.
func TestWithPullLock_SerializesConcurrentCallers(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var (
		inFlight    atomic.Int32
		maxInFlight atomic.Int32
		started     sync.WaitGroup
	)

	const N = 10
	started.Add(N)
	done := make(chan struct{}, N)

	for i := 0; i < N; i++ {
		go func() {
			started.Done()
			err := r.withPullLock(func() error {
				cur := inFlight.Add(1)
				// Track the high-water mark of concurrent critical sections.
				for {
					prev := maxInFlight.Load()
					if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
						break
					}
				}
				// Simulate doing work — long enough for the race to
				// matter if the mutex were missing.
				time.Sleep(5 * time.Millisecond)
				inFlight.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("withPullLock returned error: %v", err)
			}
			done <- struct{}{}
		}()
	}

	// Drain.
	for i := 0; i < N; i++ {
		<-done
	}

	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("max concurrent critical sections = %d, want 1 (mutex must serialize)", got)
	}
}

// TestWithPullLock_FailedCallDoesNotPoison verifies that an error from
// fn doesn't leave the mutex in a held or unrecoverable state — the
// next caller must be able to acquire the lock and run successfully.
func TestWithPullLock_FailedCallDoesNotPoison(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	wantErr := errors.New("simulated pull failure")
	if err := r.withPullLock(func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("first call: got %v, want %v", err, wantErr)
	}

	// Second call must succeed — lock was released by the deferred Unlock.
	called := false
	if err := r.withPullLock(func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("second call after failed first: %v", err)
	}
	if !called {
		t.Error("second call's fn was never invoked — lock may be stuck")
	}
}

// TestWithPullLock_PanicReleasesLock verifies that even a panic inside
// fn releases the mutex (deferred Unlock). The next caller should not
// deadlock.
func TestWithPullLock_PanicReleasesLock(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Run the panicking call inside its own goroutine so we can recover
	// independently and not abort the test. withPullLock never returns
	// because fn panics — we surface either the recovered value or any
	// surprise return value through panicDone so the test fails loudly.
	panicDone := make(chan any, 1)
	go func() {
		defer func() {
			panicDone <- recover()
		}()
		if err := r.withPullLock(func() error {
			panic("boom")
		}); err != nil {
			// Unreachable in practice: the panic should propagate
			// through withPullLock. If it doesn't, fail by routing
			// the surprise error to the recover channel.
			panic(err)
		}
	}()
	got := <-panicDone
	if got == nil {
		t.Fatal("expected panic to propagate from withPullLock")
	}

	// Try to acquire the lock again with a tight timeout. If the panic
	// failed to release the mutex, this call would block forever.
	acquired := make(chan struct{})
	go func() {
		err := r.withPullLock(func() error { return nil })
		if err != nil {
			t.Errorf("post-panic withPullLock returned %v", err)
		}
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("withPullLock blocked after a panicking call — mutex not released")
	}
}

// TestWithPullLock_ConcurrentSuccessAndFailure interleaves a failing
// call with a succeeding call to confirm the lock continues to be
// usable across the boundary. This is the spec described in item #16:
// "one failing pull doesn't poison the second."
func TestWithPullLock_ConcurrentSuccessAndFailure(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const N = 20
	wantErr := errors.New("intermittent")
	results := make([]error, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.withPullLock(func() error {
				time.Sleep(time.Millisecond)
				if i%2 == 0 {
					return wantErr
				}
				return nil
			})
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if i%2 == 0 {
			if !errors.Is(got, wantErr) {
				t.Errorf("call %d: got %v, want %v", i, got, wantErr)
			}
		} else {
			if got != nil {
				t.Errorf("call %d: got %v, want nil", i, got)
			}
		}
	}
}
