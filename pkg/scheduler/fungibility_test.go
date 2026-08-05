package scheduler

// Tests for label-set (fungibility-class) dispatch reconciliation.
//
// GitHub does not honor the runner<->job pairing ephemerd creates: it hands a
// connected JIT runner ANY queued job whose labels the runner satisfies. So
// dispatch accounting has to balance on the LABEL SET, not the job id. These
// tests pin the two places that matters:
//
//   - admitDispatch: a dispatch that sat blocked on the concurrency semaphore
//     while a sibling runner ran its job is abandoned instead of being turned
//     into a runner with nothing to do.
//   - sweepOrphanRunners: an unbound runner whose job ran elsewhere is retired
//     as soon as its label set has no uncovered queued job left, instead of
//     holding a concurrency slot for the whole orphan grace window.

import (
	"context"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

func TestLabelSetKey(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		same bool
	}{
		{"identical", []string{"self-hosted", "linux"}, []string{"self-hosted", "linux"}, true},
		{"order does not matter", []string{"self-hosted", "linux"}, []string{"linux", "self-hosted"}, true},
		{"case does not matter", []string{"Self-Hosted", "LINUX"}, []string{"self-hosted", "linux"}, true},
		{"whitespace trimmed", []string{" linux ", "self-hosted"}, []string{"linux", "self-hosted"}, true},
		{"duplicates collapse", []string{"linux", "linux", "self-hosted"}, []string{"linux", "self-hosted"}, true},
		{"empty entries dropped", []string{"linux", "", "self-hosted"}, []string{"linux", "self-hosted"}, true},
		{"different os", []string{"self-hosted", "linux"}, []string{"self-hosted", "macos"}, false},
		{"extra label is a different class", []string{"self-hosted", "linux"}, []string{"self-hosted", "linux", "gpu"}, false},
		{"both empty", nil, []string{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ka, kb := labelSetKey(tt.a), labelSetKey(tt.b)
			if (ka == kb) != tt.same {
				t.Fatalf("labelSetKey(%v)=%q vs labelSetKey(%v)=%q: same=%v, want %v",
					tt.a, ka, tt.b, kb, ka == kb, tt.same)
			}
		})
	}
}

// TestAdmitDispatch_SiblingDischargesPendingDispatch is the production
// livelock, reproduced at max_concurrent = 1.
//
// handleQueued accepts a job and hands it to a provisioning path, which blocks
// on the single concurrency slot. While it is blocked, GitHub hands the job to
// an ALREADY-DISPATCHED same-label runner, which runs it to completion. When
// the slot finally frees, the old code went right on to claim a fresh JIT
// runner for a job that had already finished — a runner nothing would ever
// bind, tearing down only when the orphan grace window (90m in production)
// expired, with the pool's only slot held the whole time.
//
// The sibling's completion must instead DISCHARGE the pending dispatch.
func TestAdmitDispatch_SiblingDischargesPendingDispatch(t *testing.T) {
	tests := []struct {
		name        string
		webhookMode bool
		satisfy     bool // simulate a sibling runner running the job while we wait
		wantClaims  int32
		wantRunning bool
	}{
		{
			name:        "job ran on a sibling while we waited for a slot",
			webhookMode: true,
			satisfy:     true,
			wantClaims:  0,
			wantRunning: false,
		},
		{
			name:        "job still queued when the slot frees",
			webhookMode: true,
			satisfy:     false,
			wantClaims:  1,
			wantRunning: true,
		},
		{
			// started is only populated from in_progress/completed webhooks;
			// in poll mode it carries no signal and must not gate dispatch.
			name:        "poll mode ignores the observed-running signal",
			webhookMode: false,
			satisfy:     true,
			wantClaims:  1,
			wantRunning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
			defer close(fake.waitBlock)
			_, dc, stopDispatch := startFakeDispatchServer(t, fake)
			defer stopDispatch()

			base := newClaimCountingProvider("github")
			prov := &reportingProvider{claimCountingProvider: base}

			s := New(Config{
				Providers:       []providers.Provider{prov},
				LinuxDispatcher: dc,
				MaxConcurrent:   1, // the production shape: one job at a time
				JobTimeout:      30 * time.Second,
				Log:             quietLogger(),
			})
			s.webhookMode = tt.webhookMode

			const jobB int64 = 900
			keyB := jobKey{Provider: prov.Name(), JobID: jobB}
			event := linuxQueuedEvent(prov, jobB)

			// Occupy the only concurrency slot, standing in for the runner
			// that is already dispatched and busy.
			s.linuxSem <- struct{}{}

			done := make(chan struct{})
			go func() {
				defer close(done)
				s.handleQueued(context.Background(), event)
			}()

			// Wait until the handler has accepted the job and is blocked on
			// the semaphore — i.e. it is past handleQueued's own guards.
			if !waitForPending(t, s, keyB) {
				t.Fatal("handler never reached the semaphore wait")
			}

			if tt.satisfy {
				// GitHub hands job B to the already-dispatched sibling runner,
				// which runs it and finishes. Neither event names a runner we
				// own, so all they do is record the observed execution.
				s.handleInProgress(providers.JobEvent{
					Provider: prov, Action: "in_progress", Repo: "myrepo",
					JobID: jobB, RunnerName: "sibling-runner",
				})
				s.handleCompleted(context.Background(), providers.JobEvent{
					Provider: prov, Action: "completed", Repo: "myrepo",
					JobID: jobB, RunnerName: "sibling-runner", Conclusion: "success",
				})
			}

			// Free the slot; the blocked handler wakes up.
			<-s.linuxSem
			<-done

			if got := base.claims.Load(); got != tt.wantClaims {
				t.Errorf("claims = %d, want %d", got, tt.wantClaims)
			}

			s.mu.Lock()
			_, running := s.running[keyB]
			_, pending := s.pending[keyB]
			s.mu.Unlock()

			if running != tt.wantRunning {
				t.Errorf("running[jobB] = %v, want %v", running, tt.wantRunning)
			}
			if pending {
				t.Error("pending[jobB] was not cleared")
			}
			// max_concurrent accounting: an abandoned dispatch must give its
			// slot back, or the pool deadlocks worse than the bug it fixes.
			if !tt.wantRunning && len(s.linuxSem) != 0 {
				t.Errorf("abandoned dispatch leaked its concurrency slot: linuxSem holds %d", len(s.linuxSem))
			}
		})
	}
}

// TestSameLabelJobs_GetExactlyOneDispatchEach guards the other direction: two
// genuinely queued same-label jobs must produce exactly two dispatches. Not
// one (the label-set reconciliation must not swallow real demand) and not
// three (no double-provisioning). Neither runner is a sweep candidate while
// both jobs are still outstanding.
func TestSameLabelJobs_GetExactlyOneDispatchEach(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	defer close(fake.waitBlock)
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		OrphanSweep:     OrphanSweepConfig{Enabled: true, Grace: 10 * time.Minute},
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobA, jobB int64 = 901, 902
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}
	keyB := jobKey{Provider: prov.Name(), JobID: jobB}

	s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobA))
	s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobB))

	if got := base.claims.Load(); got != 2 {
		t.Fatalf("two same-label queued jobs produced %d claims, want exactly 2", got)
	}

	s.mu.Lock()
	rjA, okA := s.running[keyA]
	rjB, okB := s.running[keyB]
	nRunners := len(s.runners)
	s.mu.Unlock()

	if !okA || !okB {
		t.Fatalf("both jobs should be tracked: A=%v B=%v", okA, okB)
	}
	if rjA.runnerName() == rjB.runnerName() {
		t.Errorf("both jobs share runner %q; each queued job needs its own JIT runner", rjA.runnerName())
	}
	if nRunners != 2 {
		t.Errorf("runner ledger has %d entries, want 2", nRunners)
	}

	// A redelivered queued event for A must not add a third dispatch.
	s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobA))
	if got := base.claims.Load(); got != 2 {
		t.Errorf("redelivered queued event produced a duplicate dispatch: claims = %d, want 2", got)
	}

	// Both runners are unbound but both jobs are still outstanding demand in
	// the same label set, so the sweep must leave them alone.
	s.sweepOrphanRunners()
	if names := destroyedNames(fake); len(names) != 0 {
		t.Errorf("sweep destroyed runners still covering queued demand: %v", names)
	}
}

// TestSweepOrphanRunners_LabelSetReconciliation is the sweep decision matrix.
//
// A "spare" is an unbound dispatched runner. It is DISCHARGED when the job it
// was dispatched for has been observed running somewhere else (a same-label
// sibling runner or a peer daemon took it). A discharged spare is only worth
// its concurrency slot while some other job in the SAME fungibility class is
// queued and uncovered — GitHub would hand it that job. Otherwise it is
// retired immediately rather than sitting out the grace window.
func TestSweepOrphanRunners_LabelSetReconciliation(t *testing.T) {
	linux := []string{"self-hosted", "linux"}
	macos := []string{"self-hosted", "macos"}

	type spare struct {
		name       string
		jobID      int64
		labels     []string
		age        time.Duration
		discharged bool
	}
	type queued struct {
		jobID  int64
		labels []string
		zombie bool
	}

	const grace = 10 * time.Minute

	tests := []struct {
		name       string
		spares     []spare
		queued     []queued
		wantReaped []string
		wantKept   []string
	}{
		{
			name:       "discharged spare with no demand is retired inside the grace window",
			spares:     []spare{{"r-linux", 1, linux, time.Minute, true}},
			wantReaped: []string{"r-linux"},
		},
		{
			name:     "discharged spare is kept while a same-label job is uncovered",
			spares:   []spare{{"r-linux", 1, linux, time.Minute, true}},
			queued:   []queued{{2, linux, false}},
			wantKept: []string{"r-linux"},
		},
		{
			// The cross-discharge guard: a queued macOS job is not something
			// a Linux runner can ever be handed, so it must not keep one alive.
			name:       "a different label set does not discharge",
			spares:     []spare{{"r-linux", 1, linux, time.Minute, true}},
			queued:     []queued{{2, macos, false}},
			wantReaped: []string{"r-linux"},
		},
		{
			name:     "undischarged spare inside the grace window is kept",
			spares:   []spare{{"r-linux", 1, linux, time.Minute, false}},
			wantKept: []string{"r-linux"},
		},
		{
			// Rule 1 must survive: a runner nothing ever wanted still gets
			// reaped, or genuinely unassigned runners leak forever.
			name:       "genuinely unassigned runner is still reaped after the grace window",
			spares:     []spare{{"r-linux", 1, linux, 11 * time.Minute, false}},
			wantReaped: []string{"r-linux"},
		},
		{
			// Two spares, one uncovered same-label job: keep exactly one
			// (the newest, most grace left) and retire the surplus.
			name: "surplus is retired, one spare kept per uncovered same-label job",
			spares: []spare{
				{"r-old", 1, linux, 5 * time.Minute, true},
				{"r-new", 2, linux, time.Minute, true},
			},
			queued:     []queued{{3, linux, false}},
			wantReaped: []string{"r-old"},
			wantKept:   []string{"r-new"},
		},
		{
			// A job we have given up on (over the zombie provision cap) is
			// not demand and must not keep a spare alive.
			name:       "zombie job is not demand",
			spares:     []spare{{"r-linux", 1, linux, time.Minute, true}},
			queued:     []queued{{2, linux, true}},
			wantReaped: []string{"r-linux"},
		},
		{
			// Per-class accounting: the macOS spare is kept by macOS demand,
			// the Linux spare has none and goes.
			name: "classes are reconciled independently",
			spares: []spare{
				{"r-linux", 1, linux, time.Minute, true},
				{"r-macos", 2, macos, time.Minute, true},
			},
			queued:     []queued{{3, macos, false}},
			wantReaped: []string{"r-linux"},
			wantKept:   []string{"r-macos"},
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
				OrphanSweep:     OrphanSweepConfig{Enabled: true, Grace: grace},
				Log:             quietLogger(),
			})
			s.webhookMode = true

			keys := map[string]jobKey{}
			for _, sp := range tt.spares {
				key := seedDispatchedRunner(s, prov, sp.jobID, sp.name,
					time.Now().Add(-sp.age), false, true)
				keys[sp.name] = key
				s.mu.Lock()
				s.runners[sp.name].labelSet = labelSetKey(sp.labels)
				s.seen[key] = time.Now()
				s.jobLabels[key] = labelSetKey(sp.labels)
				if sp.discharged {
					// The job this runner was brought up for was observed
					// running somewhere else.
					s.started[key] = time.Now()
				}
				s.mu.Unlock()
			}
			for _, q := range tt.queued {
				key := jobKey{Provider: prov.Name(), JobID: q.jobID}
				s.mu.Lock()
				s.seen[key] = time.Now()
				s.jobLabels[key] = labelSetKey(q.labels)
				if q.zombie {
					s.attempts[key] = maxProvisionAttempts + 1
				}
				s.mu.Unlock()
			}

			s.sweepOrphanRunners()

			destroyed := destroyedNames(fake)
			for _, name := range tt.wantReaped {
				if !destroyed[name] {
					t.Errorf("runner %q should have been retired (destroyed: %v)", name, destroyed)
				}
				s.mu.Lock()
				_, stillRunning := s.running[keys[name]]
				_, stillLedgered := s.runners[name]
				s.mu.Unlock()
				if stillRunning {
					t.Errorf("retired runner %q is still in running", name)
				}
				if stillLedgered {
					t.Errorf("retired runner %q is still in the runner ledger", name)
				}
			}
			for _, name := range tt.wantKept {
				if destroyed[name] {
					t.Errorf("runner %q was retired but should have been kept", name)
				}
				s.mu.Lock()
				_, stillRunning := s.running[keys[name]]
				s.mu.Unlock()
				if !stillRunning {
					t.Errorf("kept runner %q was dropped from running", name)
				}
			}
		})
	}
}

// waitForPending blocks until s.pending contains key (or the deadline
// elapses). A job is pending exactly while its handler has been accepted but
// has not yet taken a concurrency slot.
func waitForPending(t *testing.T, s *Scheduler, key jobKey) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, ok := s.pending[key]
		s.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}
