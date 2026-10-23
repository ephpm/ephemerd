package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/vm"
)

// TestSweepMacClones_IdleWithoutMacOS: a node with no macOS support (every
// Linux and Windows node, and a Mac before its base image is ready) never
// touches the disk.
func TestSweepMacClones_IdleWithoutMacOS(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	called := false
	s.macClones.sweep = func(string, map[string]bool, time.Duration, time.Time) vm.MacOSCloneSweep {
		called = true
		return vm.MacOSCloneSweep{}
	}
	s.sweepMacClones(time.Now())
	if called {
		t.Error("swept with no macOS VM config")
	}
}

// TestSweepMacClones_PassesGraceAndTotals: a pass sweeps the configured data
// dir with the production grace and the live set, and its results add up in
// the totals status reports.
func TestSweepMacClones_PassesGraceAndTotals(t *testing.T) {
	s := New(Config{Log: quietLogger(), MacOSVMConfig: &vm.MacOSVMConfig{DataDir: "/var/lib/ephemerd"}})
	var gotDir string
	var gotGrace time.Duration
	s.macClones.sweep = func(dir string, live map[string]bool, grace time.Duration, _ time.Time) vm.MacOSCloneSweep {
		gotDir, gotGrace = dir, grace
		if live == nil {
			t.Error("sweep got a nil live set; it must always get the live VM ids")
		}
		return vm.MacOSCloneSweep{Removed: 3, FreedBytes: 38 << 30}
	}
	s.sweepMacClones(time.Now())
	s.sweepMacClones(time.Now())
	if gotDir != "/var/lib/ephemerd" || gotGrace != vm.MacOSCloneGrace {
		t.Errorf("swept dir=%q grace=%s, want the data dir and vm.MacOSCloneGrace", gotDir, gotGrace)
	}
	if n, freed := s.macClones.totals(); n != 6 || freed != 2*(38<<30) {
		t.Errorf("totals = (%d, %d), want (6, %d)", n, freed, 2*(38<<30))
	}
}

// TestMacCloneSweeper_KickRunsAPass: SetMacOSVMConfig is the startup pass —
// it must trigger a sweep promptly, not wait for the 30 minute tick.
func TestMacCloneSweeper_KickRunsAPass(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	passes := make(chan struct{}, 4)
	s.macClones.sweep = func(string, map[string]bool, time.Duration, time.Time) vm.MacOSCloneSweep {
		passes <- struct{}{}
		return vm.MacOSCloneSweep{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runMacCloneSweeper(ctx)

	s.SetMacOSVMConfig(&vm.MacOSVMConfig{DataDir: t.TempDir()})
	select {
	case <-passes:
	case <-time.After(2 * time.Second):
		t.Fatal("SetMacOSVMConfig did not trigger a sweep")
	}

	// Kicks coalesce: a burst never blocks the caller.
	for range 10 {
		s.macClones.kickNow()
	}
}
