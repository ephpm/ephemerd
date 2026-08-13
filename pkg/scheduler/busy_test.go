package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/runnerbusy"
)

// Stand-ins for the real ground-truth check. Tests have no containerd,
// no guest VM and no runner process to introspect, so the probe is
// injected; what is under test here is what the scheduler DOES with each
// answer.
func idleProbe(context.Context, *runningJob) (runnerbusy.State, string) {
	return runnerbusy.Idle, "test"
}

func busyProbeFn(context.Context, *runningJob) (runnerbusy.State, string) {
	return runnerbusy.Busy, "test"
}

func unknownProbe(context.Context, *runningJob) (runnerbusy.State, string) {
	return runnerbusy.Unknown, "test"
}

// TestDecideReap is the veto's decision matrix as a pure function:
// nomination + busy verdict + elapsed time in, teardown decision out.
//
// The invariant it pins is the one this whole change exists for — a
// runner observed to be executing a job is never destroyed — together
// with the two bounded escapes that keep the veto from leaking a wedged
// runner forever.
func TestDecideReap(t *testing.T) {
	const (
		grace = 10 * time.Minute
		hard  = 2 * time.Hour
	)
	policy := reapPolicy{Grace: grace, HardBound: hard}
	now := time.Now()

	tests := []struct {
		name        string
		busy        runnerbusy.State
		age         time.Duration
		wantDestroy bool
		wantEscape  bool
		wantOutcome string
	}{
		{
			// The bug. A runner mid-build looks unbound because its
			// in_progress webhook has not been processed yet; the rules
			// nominate it; the check vetoes.
			name:        "busy runner inside the grace window is never destroyed",
			busy:        runnerbusy.Busy,
			age:         time.Minute,
			wantOutcome: outcomeVetoed,
		},
		{
			// Rule 1's own window is no longer authority either: a
			// runner still executing a job at the end of the grace
			// window keeps running.
			name:        "busy runner past the grace window is still not destroyed",
			busy:        runnerbusy.Busy,
			age:         grace + time.Minute,
			wantOutcome: outcomeVetoed,
		},
		{
			// What makes aggressive reaping safe: a positive idle
			// observation retires the runner immediately, without
			// waiting out any window.
			name:        "verified-idle runner is retired immediately",
			busy:        runnerbusy.Idle,
			age:         time.Second,
			wantDestroy: true,
			wantOutcome: outcomeReaped,
		},
		{
			name:        "verified-idle runner is retired past the hard bound too, without escaping",
			busy:        runnerbusy.Idle,
			age:         hard + time.Hour,
			wantDestroy: true,
			wantOutcome: outcomeReaped,
		},
		{
			// Fail-safe: a probe that cannot answer must not read as idle.
			name:        "undeterminable state inside the grace window is treated as busy",
			busy:        runnerbusy.Unknown,
			age:         time.Minute,
			wantOutcome: outcomeVetoed,
		},
		{
			// Escape 1: with no usable check we are back to exactly the
			// pre-veto behaviour rather than holding the slot forever.
			name:        "undeterminable state for the whole grace window escapes",
			busy:        runnerbusy.Unknown,
			age:         grace,
			wantDestroy: true,
			wantEscape:  true,
			wantOutcome: outcomeEscaped,
		},
		{
			// Escape 2: the only thing that overrides a POSITIVE busy
			// verdict. No legitimate job is still running this far past
			// its own deadline.
			name:        "busy runner past the hard bound escapes",
			busy:        runnerbusy.Busy,
			age:         hard,
			wantDestroy: true,
			wantEscape:  true,
			wantOutcome: outcomeEscaped,
		},
		{
			name:        "unknown runner past the hard bound escapes",
			busy:        runnerbusy.Unknown,
			age:         hard + time.Minute,
			wantDestroy: true,
			wantEscape:  true,
			wantOutcome: outcomeEscaped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideReap(reapInput{
				Runner:       "r",
				DispatchedAt: now.Add(-tt.age),
				Now:          now,
				Busy:         tt.busy,
				Policy:       policy,
			})
			if got.Destroy != tt.wantDestroy {
				t.Errorf("Destroy = %v, want %v (reason: %s)", got.Destroy, tt.wantDestroy, got.Reason)
			}
			if got.Escape != tt.wantEscape {
				t.Errorf("Escape = %v, want %v (reason: %s)", got.Escape, tt.wantEscape, got.Reason)
			}
			if got.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got.Outcome, tt.wantOutcome)
			}
			if got.Reason == "" {
				t.Error("Reason is empty; every decision must be explainable in the log")
			}
		})
	}
}

// TestReapPolicy pins how the veto's two bounds are derived: the grace
// window comes from config (with the package default as fallback), and
// the hard bound sits a margin past the job deadline — GitHub's own
// per-job ceiling when no deadline is configured.
func TestReapPolicy(t *testing.T) {
	tests := []struct {
		name          string
		grace         time.Duration
		jobTimeout    time.Duration
		wantGrace     time.Duration
		wantHardBound time.Duration
	}{
		{
			name:          "defaults",
			wantGrace:     defaultOrphanGrace,
			wantHardBound: maxJobRuntime + hardBoundMargin,
		},
		{
			name:          "configured grace and job timeout",
			grace:         2 * time.Minute,
			jobTimeout:    90 * time.Minute,
			wantGrace:     2 * time.Minute,
			wantHardBound: 90*time.Minute + hardBoundMargin,
		},
		{
			// A grace window longer than the derived hard bound would
			// let the weak (unknown) escape outlive the strong (busy)
			// one, which would be nonsense.
			name:          "hard bound never sits below the grace window",
			grace:         12 * time.Hour,
			jobTimeout:    time.Minute,
			wantGrace:     12 * time.Hour,
			wantHardBound: 12 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{
				OrphanSweep: OrphanSweepConfig{Enabled: true, Grace: tt.grace},
				JobTimeout:  tt.jobTimeout,
				Log:         quietLogger(),
			})
			got := s.reapPolicy()
			if got.Grace != tt.wantGrace {
				t.Errorf("Grace = %v, want %v", got.Grace, tt.wantGrace)
			}
			if got.HardBound != tt.wantHardBound {
				t.Errorf("HardBound = %v, want %v", got.HardBound, tt.wantHardBound)
			}
		})
	}
}

// TestSweepOrphanRunners_BusyVeto is the end-to-end statement of the
// invariant, through the real sweep: the rules nominate a runner, the
// busy check answers, and only a verified-idle runner is destroyed.
//
// Every row uses a runner that BOTH rules would retire without the veto:
// discharged (its dispatch-intent job was observed running elsewhere),
// no same-label demand left, and past the grace window.
func TestSweepOrphanRunners_BusyVeto(t *testing.T) {
	tests := []struct {
		name        string
		probe       busyProber
		age         time.Duration
		jobTimeout  time.Duration
		wantDestroy bool
	}{
		{
			name:        "busy runner is never destroyed",
			probe:       busyProbeFn,
			age:         30 * time.Minute,
			jobTimeout:  time.Hour,
			wantDestroy: false,
		},
		{
			name:        "unavailable busy check is treated as busy inside the grace window",
			probe:       unknownProbe,
			age:         time.Minute,
			jobTimeout:  time.Hour,
			wantDestroy: false,
		},
		{
			name:        "idle runner is destroyed",
			probe:       idleProbe,
			age:         30 * time.Minute,
			jobTimeout:  time.Hour,
			wantDestroy: true,
		},
		{
			// Escape hatch: a runner that has claimed to be busy well
			// past any job it could legitimately still be running is
			// wedged, and must not squat a concurrency slot forever.
			name:        "busy runner past the hard bound is destroyed anyway",
			probe:       busyProbeFn,
			age:         2 * time.Hour,
			jobTimeout:  time.Minute,
			wantDestroy: true,
		},
		{
			name:        "unavailable busy check past the grace window is destroyed anyway",
			probe:       unknownProbe,
			age:         30 * time.Minute,
			jobTimeout:  time.Hour,
			wantDestroy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDispatchServer{}
			_, dc, stopDispatch := startFakeDispatchServer(t, fake)
			defer stopDispatch()

			base := newClaimCountingProvider("github")
			prov := &reportingProvider{claimCountingProvider: base}

			s := New(Config{
				Providers:       []providers.Provider{prov},
				LinuxDispatcher: dc,
				JobTimeout:      tt.jobTimeout,
				OrphanSweep:     OrphanSweepConfig{Enabled: true, Grace: 10 * time.Minute},
				Log:             quietLogger(),
			})
			s.webhookMode = true
			s.busyProbe = tt.probe

			key := seedDispatchedRunner(s, prov, 1, "r-1", time.Now().Add(-tt.age), false, true)
			s.mu.Lock()
			s.seen[key] = time.Now()
			s.jobLabels[key] = labelSetKey([]string{"self-hosted", "linux"})
			s.runners["r-1"].labelSet = labelSetKey([]string{"self-hosted", "linux"})
			// Discharged: the job it was dispatched for was observed
			// running somewhere else, so rule 2 nominates it too.
			s.started[key] = time.Now()
			s.mu.Unlock()

			s.sweepOrphanRunners()

			if tt.wantDestroy {
				waitForDestroy(t, fake, "r-1")
			}
			destroyed := destroyedNames(fake)["r-1"]
			if destroyed != tt.wantDestroy {
				t.Errorf("runner destroyed = %v, want %v", destroyed, tt.wantDestroy)
			}

			s.mu.Lock()
			_, stillRunning := s.running[key]
			_, stillLedgered := s.runners["r-1"]
			s.mu.Unlock()
			if stillRunning == tt.wantDestroy {
				t.Errorf("running entry present = %v, want %v", stillRunning, !tt.wantDestroy)
			}
			if stillLedgered == tt.wantDestroy {
				t.Errorf("ledger entry present = %v, want %v", stillLedgered, !tt.wantDestroy)
			}
		})
	}
}

// TestSweepOrphanRunners_BindingRaceDuringProbe pins the reason the veto
// runs with the scheduler mutex released and re-validates afterwards.
//
// The probe does I/O, so an in_progress webhook can land while it is in
// flight — which is precisely the delivery skew that produced the
// original bug. A runner that gets bound in that window must survive its
// own nomination even if the probe said "idle" a moment earlier.
func TestSweepOrphanRunners_BindingRaceDuringProbe(t *testing.T) {
	fake := &fakeDispatchServer{}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		OrphanSweep:     OrphanSweepConfig{Enabled: true, Grace: 10 * time.Minute},
		Log:             quietLogger(),
	})
	s.webhookMode = true

	key := seedDispatchedRunner(s, prov, 1, "r-1", time.Now().Add(-30*time.Minute), false, true)

	// The probe answers "idle", then the webhook arrives: exactly the
	// interleaving the nominate/veto split has to survive.
	s.busyProbe = func(context.Context, *runningJob) (runnerbusy.State, string) {
		s.handleInProgress(providers.JobEvent{
			Provider:   prov,
			Action:     "in_progress",
			JobID:      777,
			Repo:       "myrepo",
			RunnerName: "r-1",
		})
		return runnerbusy.Idle, "test"
	}

	s.sweepOrphanRunners()
	time.Sleep(50 * time.Millisecond)

	if destroyedNames(fake)["r-1"] {
		t.Error("runner bound while the busy check was in flight was destroyed anyway")
	}
	s.mu.Lock()
	_, stillRunning := s.running[key]
	s.mu.Unlock()
	if !stillRunning {
		t.Error("runner bound during the probe was dropped from running")
	}
}

// TestProbeLocalBusy_UnavailablePathsAreUnknown pins the fail-safe
// contract on the local probe: every shape it cannot introspect reports
// Unknown with an explicit reason. None may fall through to Idle, which
// would be indistinguishable from "verified not running a job".
func TestProbeLocalBusy_UnavailablePathsAreUnknown(t *testing.T) {
	s := New(Config{Log: quietLogger()})

	tests := []struct {
		name       string
		rj         *runningJob
		wantSource string
	}{
		{
			// Runner lives in the Linux sidecar VM, behind the dispatch
			// gRPC boundary. The provider busy flag covers this path.
			name:       "dispatched to the linux sidecar VM",
			rj:         &runningJob{dispatched: "runner-x"},
			wantSource: "dispatched",
		},
		{
			name:       "native runner without a busy hook",
			rj:         &runningJob{nativeRunner: stopOnly{}},
			wantSource: "native-process",
		},
		{
			name:       "nothing to introspect",
			rj:         &runningJob{},
			wantSource: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, source, err := s.probeLocalBusy(context.Background(), tt.rj)
			if state != runnerbusy.Unknown {
				t.Errorf("state = %v, want unknown — an unavailable probe must never read as idle", state)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
			if !errors.Is(err, runnerbusy.ErrUnsupported) {
				t.Errorf("err = %v, want ErrUnsupported", err)
			}
		})
	}
}

type stopOnly struct{}

func (stopOnly) Stop() {}

// TestProbeRunnerBusy_FallsBackToProvider pins the second layer: when the
// local probe cannot see the runner, the provider's own busy flag decides,
// and a provider error still fails safe to Unknown.
func TestProbeRunnerBusy_FallsBackToProvider(t *testing.T) {
	tests := []struct {
		name  string
		busy  bool
		err   error
		want  runnerbusy.State
		wants string
	}{
		{name: "provider says busy", busy: true, want: runnerbusy.Busy, wants: "provider"},
		{name: "provider says idle", want: runnerbusy.Idle, wants: "provider"},
		{name: "provider errors", err: errors.New("rate limited"), want: runnerbusy.Unknown, wants: "dispatched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{Log: quietLogger()})
			prov := &busyReportingProvider{
				claimCountingProvider: newClaimCountingProvider("github"),
				busy:                  tt.busy,
				err:                   tt.err,
			}
			rj := &runningJob{
				dispatched: "runner-x",
				provider:   prov,
				claim:      &providers.Claim{RunnerID: 1, RunnerName: "runner-x", Repo: "myrepo"},
			}
			state, source := s.probeRunnerBusy(context.Background(), rj)
			if state != tt.want {
				t.Errorf("state = %v, want %v", state, tt.want)
			}
			if source != tt.wants {
				t.Errorf("source = %q, want %q", source, tt.wants)
			}
		})
	}
}

// busyReportingProvider implements providers.RunnerBusyReporter over the
// shared test provider.
type busyReportingProvider struct {
	*claimCountingProvider
	busy bool
	err  error
}

func (p *busyReportingProvider) RunnerBusy(context.Context, *providers.Claim) (bool, error) {
	return p.busy, p.err
}
