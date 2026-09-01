package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// discardLogger is shared from images_test.go.

// Delays in these tests are kept tiny (microseconds) so the retry loop's
// select/timer path is exercised without real waiting.
const testDelay = 10 * time.Microsecond

func TestRetryInit_SucceedsFirstTry(t *testing.T) {
	calls := 0
	v, err := retryInit(context.Background(), 5, testDelay, discardLogger(), "thing", func() (int, error) {
		calls++
		return 42, nil
	})
	if err != nil {
		t.Fatalf("retryInit: %v", err)
	}
	if v != 42 || calls != 1 {
		t.Errorf("v=%d calls=%d, want v=42 calls=1", v, calls)
	}
}

func TestRetryInit_FailsThenSucceeds(t *testing.T) {
	calls := 0
	v, err := retryInit(context.Background(), 10, testDelay, discardLogger(), "thing", func() (string, error) {
		calls++
		if calls < 4 {
			return "", errors.New("not ready")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("retryInit: %v", err)
	}
	if v != "ok" || calls != 4 {
		t.Errorf("v=%q calls=%d, want v=ok calls=4", v, calls)
	}
}

func TestRetryInit_ExhaustsAttemptsReturnsLastError(t *testing.T) {
	calls := 0
	last := errors.New("still broken")
	_, err := retryInit(context.Background(), 3, testDelay, discardLogger(), "thing", func() (int, error) {
		calls++
		if calls == 3 {
			return 0, last
		}
		return 0, errors.New("earlier failure")
	})
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	// The LAST error must come back untouched — it is what self-documents a
	// persistent misconfig after the retry window.
	if !errors.Is(err, last) {
		t.Errorf("err = %v, want the final attempt's error", err)
	}
}

func TestRetryInit_CtxCancelStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	// Long delay so a missed cancellation would make the test hang past its
	// deadline rather than pass by luck.
	_, err := retryInit(ctx, 30, time.Hour, discardLogger(), "thing", func() (int, error) {
		calls++
		cancel() // cancel while the loop is about to sleep
		return 0, errors.New("not ready")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry after cancellation)", calls)
	}
}

func TestRetryInit_LastAttemptDoesNotSleep(t *testing.T) {
	// With a huge delay and attempts=1, retryInit must return immediately —
	// the failure of the final attempt never waits.
	start := time.Now()
	_, err := retryInit(context.Background(), 1, time.Hour, discardLogger(), "thing", func() (int, error) {
		return 0, errors.New("nope")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("final attempt slept %v; must not sleep after the last try", elapsed)
	}
}
