package upgrade

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errRestart is what a restart request that could not even be handed to the
// service manager returns (e.g. the helper failed to spawn).
var errRestart = errors.New("could not reach the service manager")

// waitFor polls cond until it holds or the deadline passes. Used instead of a
// fixed sleep so the timing-sensitive supervisor tests stay fast and stable.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestSuperviseRestart covers the whole decision the supervisor exists to
// make: did the restart we asked for actually take effect, and if not, does
// the failure path fire exactly once?
//
// "Took effect" is modelled the way it happens in production — the daemon
// starts shutting down — because a real restart kills the process and there
// is nothing else left to observe.
func TestSuperviseRestart(t *testing.T) {
	const (
		delay    = time.Millisecond
		watchdog = 20 * time.Millisecond
	)

	tests := []struct {
		name string
		// restartErr is returned by attempt N (index N-1); a short slice
		// repeats its last element.
		restartErr []error
		// shutdownAfter, when >= 0, closes the shutdown channel that long
		// after the supervisor starts. Negative means "never shuts down".
		shutdownAfter time.Duration
		wantAttempts  int32
		wantFailed    bool
	}{
		{
			// The healthy case: the service manager takes us down.
			name:          "restart takes effect",
			restartErr:    []error{nil},
			shutdownAfter: 5 * time.Millisecond,
			wantAttempts:  1,
			wantFailed:    false,
		},
		{
			// The v0.1.8 incident, minus the fix: the request is accepted
			// and nothing happens. The node must not stay cordoned.
			name:          "restart is accepted but never happens",
			restartErr:    []error{nil},
			shutdownAfter: -1,
			wantAttempts:  restartAttempts,
			wantFailed:    true,
		},
		{
			// The helper could not be spawned at all.
			name:          "restart request always fails",
			restartErr:    []error{errRestart},
			shutdownAfter: -1,
			wantAttempts:  restartAttempts,
			wantFailed:    true,
		},
		{
			// A transient spawn failure must not doom the upgrade: the retry
			// exists precisely because a restart that had worked would
			// already have killed us.
			// Long enough to land after the retry (delay + the shortened
			// post-failure wait), short enough to precede a third.
			name:          "first request fails, retry takes effect",
			restartErr:    []error{errRestart, nil},
			shutdownAfter: 30 * time.Millisecond,
			wantAttempts:  2,
			wantFailed:    false,
		},
		{
			// Shutdown beat the initial flush delay (another upgrade, a
			// SIGTERM, an operator restart): we must not restart on top of a
			// shutdown already in progress, nor call it a failure.
			name:          "shutdown during the pre-restart delay",
			restartErr:    []error{nil},
			shutdownAfter: 0,
			wantAttempts:  0,
			wantFailed:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts int32
			shutdown := make(chan struct{})
			if tt.shutdownAfter >= 0 {
				time.AfterFunc(tt.shutdownAfter, func() { close(shutdown) })
			}

			var mu sync.Mutex
			var failedWith error
			var failedCount int

			done := make(chan struct{})
			go func() {
				defer close(done)
				superviseRestart(restartSupervisor{
					restart: func() error {
						n := atomic.AddInt32(&attempts, 1)
						i := int(n) - 1
						if i >= len(tt.restartErr) {
							i = len(tt.restartErr) - 1
						}
						return tt.restartErr[i]
					},
					delay:    delay,
					watchdog: watchdog,
					shutdown: shutdown,
					onFailed: func(err error) {
						mu.Lock()
						defer mu.Unlock()
						failedCount++
						failedWith = err
					},
				})
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("superviseRestart did not return")
			}

			if got := atomic.LoadInt32(&attempts); got != tt.wantAttempts {
				t.Errorf("restart attempts = %d, want %d", got, tt.wantAttempts)
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.wantFailed {
				if failedCount != 1 {
					t.Errorf("onFailed called %d times, want exactly 1", failedCount)
				}
				if failedWith == nil {
					t.Error("onFailed called with a nil cause; the operator needs to know why")
				}
			} else if failedCount != 0 {
				t.Errorf("onFailed called %d times on a restart that took effect, want 0", failedCount)
			}
		})
	}
}

func TestWaitUnlessShutdown(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	open := make(chan struct{})

	tests := []struct {
		name     string
		d        time.Duration
		shutdown <-chan struct{}
		want     bool
	}{
		{"waits out the duration", time.Millisecond, open, true},
		{"nil channel never fires", time.Millisecond, nil, true},
		{"already shut down", 50 * time.Millisecond, closed, false},
		{"zero duration, still running", 0, open, true},
		{"zero duration, already shut down", 0, closed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waitUnlessShutdown(tt.d, tt.shutdown); got != tt.want {
				t.Errorf("waitUnlessShutdown = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRun_RestartNeverHappensUncordons is the safety property that matters
// most: an upgrade that swaps the binary and then fails to restart must put
// the node back into service. A node that is drained AND running old code
// looks healthy (service Running, status ok) while silently accepting no
// work — strictly worse than never having attempted the upgrade.
func TestRun_RestartNeverHappensUncordons(t *testing.T) {
	tests := []struct {
		name       string
		restartErr error
	}{
		{"service manager accepted the request but nothing happened", nil},
		{"restart request could not be made at all", errRestart},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goos, goarch := "linux", "amd64"
			if runtime.GOOS == "linux" {
				goos = "windows" // non-native: skips the --version probe
			}
			const target = "v0.1.7"
			newBin := []byte("NEW-BINARY-" + target)
			fr := newFakeRelease(t, target, goos, goarch, newBin, false)
			install := setupInstall(t)

			drainer := &fakeDrainer{counts: []int{0}}
			err := Run(context.Background(), RunOptions{
				TargetVersion:   target,
				CurrentVersion:  "v0.1.6",
				BaseURLOverride: fr.server.URL,
				Drainer:         drainer,
				DrainPoll:       time.Millisecond,
				InstallPath:     install,
				GOOS:            goos,
				GOARCH:          goarch,
				Restart:         func() error { return tt.restartErr },
				RestartDelay:    time.Millisecond,
				RestartWatchdog: 10 * time.Millisecond,
				// No Shutdown channel: this daemon never goes down, which is
				// exactly the stuck state being reproduced.
			}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			// Run itself succeeds — the swap really did happen and the caller
			// is told RESTARTING. The failure surfaces afterwards.
			if !drainer.wasCordoned() {
				t.Fatal("upgrade did not cordon before swapping")
			}
			if !waitFor(t, 5*time.Second, func() bool { return drainer.uncordonCount() == 1 }) {
				t.Fatalf("node left DRAINED after a failed restart (uncordoned %d times, want 1)", drainer.uncordonCount())
			}

			// And it stays un-cordoned exactly once — no flapping.
			time.Sleep(50 * time.Millisecond)
			if got := drainer.uncordonCount(); got != 1 {
				t.Errorf("uncordoned %d times, want exactly 1", got)
			}

			// The verified new binary is deliberately NOT rolled back: it is
			// the one component here that was checksum-verified and probed,
			// and leaving it in place means the next restart — automatic or
			// manual — completes the upgrade.
			got, _ := os.ReadFile(install)
			if string(got) != string(newBin) {
				t.Errorf("installed binary = %q, want the new binary left in place for the next restart", got)
			}
			if old, err := os.ReadFile(install + ".old"); err != nil || string(old) != "OLD-BINARY" {
				t.Errorf(".old rollback copy = %q err=%v, want OLD-BINARY", old, err)
			}
		})
	}
}

// TestRun_RestartTakesEffectStaysCordoned is the mirror image: when the
// daemon really is going down, the cordon must survive. Un-cordoning on the
// way out would let the node claim a job it is about to abandon.
func TestRun_RestartTakesEffectStaysCordoned(t *testing.T) {
	goos, goarch := "linux", "amd64"
	if runtime.GOOS == "linux" {
		goos = "windows"
	}
	const target = "v0.1.7"
	fr := newFakeRelease(t, target, goos, goarch, []byte("NEW"), false)
	install := setupInstall(t)

	shutdown := make(chan struct{})
	drainer := &fakeDrainer{counts: []int{0}}

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.1.6",
		BaseURLOverride: fr.server.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		Restart:         func() error { close(shutdown); return nil },
		RestartDelay:    time.Millisecond,
		RestartWatchdog: time.Second,
		Shutdown:        shutdown,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Give the supervisor room to (wrongly) fire.
	time.Sleep(100 * time.Millisecond)
	if got := drainer.uncordonCount(); got != 0 {
		t.Errorf("uncordoned %d times while the restart was taking effect, want 0", got)
	}
}

// TestRun_UndrainsOnEveryPreSwapFailure walks the abort paths that exist
// before the binary is touched. Each one must leave the node claiming jobs on
// the old binary — the cordon is never the thing left behind.
func TestRun_UndrainsOnEveryPreSwapFailure(t *testing.T) {
	nonNative := func() (string, string) {
		if runtime.GOOS == "linux" {
			return "windows", "amd64"
		}
		return "linux", "amd64"
	}

	tests := []struct {
		name    string
		mutate  func(o *RunOptions, t *testing.T)
		corrupt bool
		native  bool
	}{
		{
			name:    "checksum mismatch",
			corrupt: true,
		},
		{
			name:   "staged binary reports the wrong version",
			native: true,
			mutate: func(o *RunOptions, _ *testing.T) {
				o.Probe = func(string) (string, error) { return "v0.0.1", nil }
			},
		},
		{
			name:   "staged binary will not execute",
			native: true,
			mutate: func(o *RunOptions, _ *testing.T) {
				o.Probe = func(string) (string, error) { return "", errors.New("exec format error") }
			},
		},
		{
			name: "release asset is missing",
			mutate: func(o *RunOptions, _ *testing.T) {
				o.BaseURLOverride += "/nope"
			},
		},
		{
			name: "staging dir cannot be created",
			mutate: func(o *RunOptions, t *testing.T) {
				// A regular file where the staging dir must go.
				blocker := o.InstallPath + ".blocked"
				if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				o.StageDir = blocker
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goos, goarch := nonNative()
			if tt.native {
				goos, goarch = runtime.GOOS, runtime.GOARCH
			}
			const target = "v0.1.7"
			fr := newFakeRelease(t, target, goos, goarch, []byte("NEW"), tt.corrupt)
			install := setupInstall(t)
			drainer := &fakeDrainer{counts: []int{0}}

			var restarted int32
			opts := RunOptions{
				TargetVersion:   target,
				CurrentVersion:  "v0.1.6",
				BaseURLOverride: fr.server.URL,
				Drainer:         drainer,
				DrainPoll:       time.Millisecond,
				InstallPath:     install,
				GOOS:            goos,
				GOARCH:          goarch,
				Restart:         func() error { atomic.AddInt32(&restarted, 1); return nil },
				RestartDelay:    time.Millisecond,
				RestartWatchdog: 10 * time.Millisecond,
			}
			if tt.mutate != nil {
				tt.mutate(&opts, t)
			}

			emit, states := collectEmit()
			if err := Run(context.Background(), opts, emit); err == nil {
				t.Fatal("expected an error")
			}

			if !drainer.wasCordoned() {
				t.Fatal("upgrade did not cordon, so this case proves nothing about un-draining")
			}
			if got := drainer.uncordonCount(); got != 1 {
				t.Errorf("uncordoned %d times after a failed upgrade, want exactly 1", got)
			}
			if !containsState(states(), StateFailed) {
				t.Error("no FAILED progress emitted; the caller would not know it failed")
			}
			// The live binary must be untouched and no restart scheduled.
			if got, _ := os.ReadFile(install); string(got) != "OLD-BINARY" {
				t.Errorf("live binary modified on a pre-swap failure: %q", got)
			}
			time.Sleep(30 * time.Millisecond)
			if atomic.LoadInt32(&restarted) != 0 {
				t.Error("a restart was requested despite the upgrade aborting before the swap")
			}
		})
	}
}

// TestRun_NoDrainerDoesNotPanicOnRestartFailure guards the un-drain path
// against the configurations that have nothing to un-drain: --no-drain, and a
// caller that supplied no Drainer at all.
func TestRun_NoDrainerDoesNotPanicOnRestartFailure(t *testing.T) {
	tests := []struct {
		name    string
		drainer Drainer
		noDrain bool
	}{
		{"no drainer supplied", nil, false},
		{"drainer supplied but draining skipped", &fakeDrainer{counts: []int{0}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goos, goarch := "linux", "amd64"
			if runtime.GOOS == "linux" {
				goos = "windows"
			}
			const target = "v0.1.7"
			fr := newFakeRelease(t, target, goos, goarch, []byte("NEW"), false)

			err := Run(context.Background(), RunOptions{
				TargetVersion:   target,
				CurrentVersion:  "v0.1.6",
				BaseURLOverride: fr.server.URL,
				NoDrain:         tt.noDrain,
				Drainer:         tt.drainer,
				InstallPath:     setupInstall(t),
				GOOS:            goos,
				GOARCH:          goarch,
				Restart:         func() error { return errRestart },
				RestartDelay:    time.Millisecond,
				RestartWatchdog: 5 * time.Millisecond,
			}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The supervisor runs on its own goroutine; give it time to reach
			// onFailed, which must be a no-op rather than a nil-map panic.
			time.Sleep(80 * time.Millisecond)
			if d, ok := tt.drainer.(*fakeDrainer); ok && d.uncordonCount() != 0 {
				t.Errorf("uncordoned %d times without ever cordoning", d.uncordonCount())
			}
		})
	}
}

func TestRestartOptionsDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   RestartOptions
		want RestartOptions
	}{
		{
			name: "all zero takes every default",
			in:   RestartOptions{},
			want: RestartOptions{DefaultRestartStopTimeout, DefaultRestartStartTimeout, defaultRestartPoll},
		},
		{
			name: "explicit values win",
			in:   RestartOptions{StopTimeout: time.Second, StartTimeout: 2 * time.Second, Poll: 3 * time.Second},
			want: RestartOptions{time.Second, 2 * time.Second, 3 * time.Second},
		},
		{
			name: "negative is treated as unset",
			in:   RestartOptions{StopTimeout: -1, StartTimeout: -1, Poll: -1},
			want: RestartOptions{DefaultRestartStopTimeout, DefaultRestartStartTimeout, defaultRestartPoll},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.withDefaults(); got != tt.want {
				t.Errorf("withDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestManualRestartHintIsActionable(t *testing.T) {
	// The hint goes straight into an operator-facing failure message, so an
	// empty or placeholder value would be worse than useless.
	if h := ManualRestartHint(); len(h) < len("restart") || h != manualRestartHint {
		t.Errorf("ManualRestartHint() = %q, want a real command for %s", h, runtime.GOOS)
	}
}
