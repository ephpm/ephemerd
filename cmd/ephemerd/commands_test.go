package main

import "testing"

// TestDrainStrategyFor pins the platform routing for the default (no --wait)
// drain. Windows has no signals — os.Process.Signal always fails there — so
// it must take the control-socket route; POSIX keeps the SIGTERM path it has
// always had.
func TestDrainStrategyFor(t *testing.T) {
	if got := drainStrategyFor("windows"); got != drainControl {
		t.Errorf("windows drain strategy = %v, want drainControl (SIGTERM cannot work there)", got)
	}
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if got := drainStrategyFor(goos); got != drainSignal {
			t.Errorf("%s drain strategy = %v, want drainSignal", goos, got)
		}
	}
}
