package networking

import (
	"context"
	"errors"
	"testing"
	"time"
)

func shortRetryDelay(t *testing.T) {
	t.Helper()
	old := setupRetryDelay
	setupRetryDelay = time.Millisecond
	t.Cleanup(func() { setupRetryDelay = old })
}

// The EINTR seen on ducks (2026-10-22) arrives as text from the plugin
// process, not as a wrapped errno.
var errEINTR = errors.New(`plugin type="loopback" failed (add): interrupted system call`)

func TestRetrySetup_RecoversFromEINTR(t *testing.T) {
	shortRetryDelay(t)
	calls, undos := 0, 0
	got, err := retrySetup(context.Background(), func() (string, error) {
		calls++
		if calls == 1 {
			return "", errEINTR
		}
		return "10.88.0.5", nil
	}, func() { undos++ })
	if err != nil {
		t.Fatalf("retrySetup: %v", err)
	}
	if got != "10.88.0.5" || calls != 2 || undos != 1 {
		t.Errorf("got %q after %d calls and %d undos; want 10.88.0.5 after 2 calls and 1 undo", got, calls, undos)
	}
}

func TestRetrySetup_DoesNotRetryOtherErrors(t *testing.T) {
	shortRetryDelay(t)
	calls := 0
	want := errors.New(`plugin type="bridge" failed (add): no IP addresses available in range set`)
	_, err := retrySetup(context.Background(), func() (string, error) {
		calls++
		return "", want
	}, func() { t.Error("undo ran for a non-transient error") })
	if !errors.Is(err, want) || calls != 1 {
		t.Errorf("got err=%v after %d calls; want the original error after 1 call", err, calls)
	}
}

func TestRetrySetup_GivesUp(t *testing.T) {
	shortRetryDelay(t)
	calls := 0
	_, err := retrySetup(context.Background(), func() (string, error) {
		calls++
		return "", errEINTR
	}, func() {})
	if !errors.Is(err, errEINTR) || calls != setupAttempts {
		t.Errorf("got err=%v after %d calls; want EINTR after %d calls", err, calls, setupAttempts)
	}
}
