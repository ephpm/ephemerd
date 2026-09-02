//go:build windows

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/vm"
)

// TestLinuxVMStartLadderParams pins the retry budget.
//
// The bug: vm.StartLinuxVM was a single shot, so one transient cold-boot
// vmcompute/Hyper-V error stripped Linux capacity from the node for the whole
// daemon uptime while it kept reporting healthy. The ladder has to be long
// enough to outlast the service-start race after a host reboot, and short
// enough that a genuinely misconfigured host reaches the give-up log (and
// that shutdown mid-ladder is not a long stall) instead of retrying forever.
func TestLinuxVMStartLadderParams(t *testing.T) {
	if linuxVMStartAttempts < 2 {
		t.Fatalf("attempts = %d — a single shot is the bug this fixes", linuxVMStartAttempts)
	}
	if linuxVMStartDelay <= 0 {
		t.Fatalf("delay = %v, want a positive backoff", linuxVMStartDelay)
	}
	// Worst-case wall time spent sleeping: the final attempt never sleeps.
	window := time.Duration(linuxVMStartAttempts-1) * linuxVMStartDelay
	if window < 30*time.Second {
		t.Errorf("retry window %v is under 30s — too short to ride out vmcompute settling after a host reboot", window)
	}
	if window > 5*time.Minute {
		t.Errorf("retry window %v is over 5m — a broken host must reach the give-up log, and shutdown waits on this ladder", window)
	}
}

// TestLinuxVMStartLadder_FailSoftContract exercises retryInit exactly as the
// Linux-sidecar goroutine instantiates it (T = vm.LinuxVM, an interface).
// Exhausting the ladder must yield a nil VM plus the LAST error — the
// goroutine logs that and returns, leaving the node serving Windows jobs.
// Making this path fatal would turn a missing sidecar into a Windows-CI
// outage, which is strictly worse than the Linux jobs queueing.
func TestLinuxVMStartLadder_FailSoftContract(t *testing.T) {
	last := errors.New("vmcompute: the service is not ready")
	calls := 0
	got, err := retryInit(context.Background(), 3, time.Millisecond, discardLogger(), "Linux VM", func() (vm.LinuxVM, error) {
		calls++
		return nil, last
	})
	if !errors.Is(err, last) {
		t.Fatalf("err = %v, want the last start error so the give-up log names the real cause", err)
	}
	if got != nil {
		t.Errorf("VM = %v, want nil on give-up", got)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 — the ladder must actually retry", calls)
	}
}

// TestLinuxVMStartLadder_CancelUnblocksShutdown: cleanup cancels the start
// ctx before waiting on linuxVMDone. Without honoring cancellation, stopping
// the daemon mid-ladder would block for the ladder's remaining budget.
func TestLinuxVMStartLadder_CancelUnblocksShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	_, err := retryInit(ctx, linuxVMStartAttempts, linuxVMStartDelay, discardLogger(), "Linux VM", func() (vm.LinuxVM, error) {
		cancel() // stand in for cleanup()'s cancelVMStart
		return nil, errors.New("not ready")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed >= linuxVMStartDelay {
		t.Errorf("ladder took %v after cancellation; shutdown would stall for the retry budget", elapsed)
	}
}
