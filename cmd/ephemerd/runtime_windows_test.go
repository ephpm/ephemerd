//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/vm"
)

// TestLinuxVMStartLadderParams pins the retry budget.
//
// The bug it started from: vm.StartLinuxVM was a single shot, so one
// transient cold-boot vmcompute/Hyper-V error stripped Linux capacity from
// the node for the whole daemon uptime while it kept reporting healthy.
//
// The bug this test USED TO ENSHRINE: it bounded (attempts-1) x delay and
// called the result the "retry window", which counts only the SLEEPS. A
// failing vm.StartLinuxVM attempt blocks for roughly ten minutes on its own
// (pkg/vm/linuxvm_windows.go: discoverIP 60s, waitForContainerd ~360s, the
// dispatch wait ~270s, Stop ~11s), so "10 x 6s" was about ninety minutes of
// real time, not one minute. An invariant expressed in sleeps cannot bound a
// ladder whose cost is dominated by the attempts.
//
// So the real bound is wall-clock: linuxVMStartBudget, enforced as a deadline
// on the ctx retryInit runs under.
func TestLinuxVMStartLadderParams(t *testing.T) {
	if linuxVMStartAttempts < 2 {
		t.Fatalf("attempts = %d — a single shot is the bug this fixes", linuxVMStartAttempts)
	}
	if linuxVMStartDelay <= 0 {
		t.Fatalf("delay = %v, want a positive backoff", linuxVMStartDelay)
	}

	// The ladder must be bounded by something that does NOT assume attempts
	// are cheap. A budget shorter than a single worst-case attempt would
	// make the ladder effectively single-shot again; one much longer lets a
	// misconfigured host churn for hours instead of reaching the give-up
	// log.
	const worstCaseAttempt = 11 * time.Minute
	if linuxVMStartBudget < worstCaseAttempt {
		t.Errorf("budget %v is under one worst-case attempt (%v) — the ladder degenerates to a single shot on a slow host",
			linuxVMStartBudget, worstCaseAttempt)
	}
	if linuxVMStartBudget > 45*time.Minute {
		t.Errorf("budget %v is over 45m — a host that cannot boot the sidecar must reach the give-up log, not retry for hours", linuxVMStartBudget)
	}

	// Sleeps must be a rounding error against the budget, or the ladder is
	// spending its bound waiting rather than trying.
	if sleeps := time.Duration(linuxVMStartAttempts-1) * linuxVMStartDelay; sleeps > linuxVMStartBudget/4 {
		t.Errorf("sleeps total %v, more than a quarter of the %v budget", sleeps, linuxVMStartBudget)
	}
}

// TestLinuxVMStartupIsNotOnTheBlockingPath pins the property that actually
// mattered: NONE of the ladder's cost may land on daemon startup.
//
// It used to. cmd/ephemerd/main.go called waitDispatch() — a bare
// `<-linuxVMDone` — before the webhook tunnel and before scheduler.New, so a
// Windows host whose Hyper-V stack was unhappy sat for up to the whole ladder
// as a daemon with no scheduler and no webhook receiver, unable to run the
// Windows jobs it was perfectly capable of running. A Linux sidecar is extra
// capacity and must never gate the host's primary capacity.
func TestLinuxVMStartupIsNotOnTheBlockingPath(t *testing.T) {
	if linuxDispatchStartupWait >= linuxVMStartDelay {
		t.Errorf("startup wait %v is not clearly shorter than even one retry delay (%v); startup is still shaped by the ladder",
			linuxDispatchStartupWait, linuxVMStartDelay)
	}
	if linuxDispatchStartupWait > 15*time.Second {
		t.Errorf("startup wait %v — the daemon must become schedulable in seconds, not on the sidecar's timeline", linuxDispatchStartupWait)
	}
	// Shutdown must be able to walk away from the ladder well inside the
	// Windows SCM's 30s stop timeout.
	if linuxVMShutdownGrace > 10*time.Second {
		t.Errorf("shutdown grace %v risks the SCM hard-killing the service mid-teardown (30s limit)", linuxVMShutdownGrace)
	}
	if linuxVMShutdownGrace <= 0 {
		t.Errorf("shutdown grace %v — a clean ladder exit should still get a moment to happen", linuxVMShutdownGrace)
	}
}

// TestLinuxVMStartLadder_BudgetStopsItBeforeTheNextAttempt: the ctx deadline
// is what bounds the ladder, and retryInit must consult it BEFORE calling fn.
// Checking only between attempts meant an expired budget still bought one
// more full (multi-minute) attempt.
func TestLinuxVMStartLadder_BudgetStopsItBeforeTheNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	start := time.Now()
	_, err := retryInit(ctx, linuxVMStartAttempts, linuxVMStartDelay, discardLogger(), "Linux VM", func() (vm.LinuxVM, error) {
		calls++
		// Stands in for the budget expiring inside a long attempt.
		cancel()
		return nil, errors.New("vmcompute: the service is not ready")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times after the budget expired, want 1 — each extra attempt costs minutes, not the retry delay", calls)
	}
	if elapsed := time.Since(start); elapsed >= linuxVMStartDelay {
		t.Errorf("ladder took %v to give up; it must not sleep out the backoff after the budget is gone", elapsed)
	}
	// The give-up error must still name the real cause, or the operator
	// gets a bare "context canceled" with no diagnosis.
	if !strings.Contains(err.Error(), "vmcompute") {
		t.Errorf("err = %q, want it to carry the last start error", err)
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
