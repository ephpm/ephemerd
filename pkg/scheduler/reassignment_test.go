package scheduler

// Tests for the runner-reassignment race: GitHub treats registered JIT
// runners with matching labels as fungible, so the runner ephemerd
// dispatches "for" job A can be assigned job B by GitHub's scheduler.
// Teardown must follow the OBSERVED assignment (the runner named in the
// in_progress / completed webhook), never the dispatch intent —
// destroying job.dispatched on job A's completion used to kill whatever
// job that runner was actually executing (observed in production as
// jobs failing at ~10m01s with no logs after their runner vanished).

import (
	"context"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// reportingProvider wraps claimCountingProvider with
// providers.RunnerNameReporter so runners dispatched through it are
// eligible for the orphan sweep (like the real GitHub provider).
type reportingProvider struct {
	*claimCountingProvider
}

func (p *reportingProvider) ReportsRunnerNames() bool { return true }

var _ providers.RunnerNameReporter = (*reportingProvider)(nil)

// dispatchLinuxJob drives a queued Linux job through handleQueued
// against the fake dispatch server and returns the name of the runner
// that was dispatched for it.
func dispatchLinuxJob(t *testing.T, s *Scheduler, prov providers.Provider, jobID int64) string {
	t.Helper()

	event := providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    jobID,
		Labels:   []string{"self-hosted", "linux"},
	}
	s.handleQueued(context.Background(), event)

	s.mu.Lock()
	rj, ok := s.running[keyFor(event)]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("job %d not tracked in running after handleQueued", jobID)
	}
	if rj.dispatched == "" {
		t.Fatalf("job %d has no dispatched runner name", jobID)
	}
	return rj.dispatched
}

// destroyedNames snapshots the ids the fake dispatch server has been
// asked to destroy so far.
func destroyedNames(fake *fakeDispatchServer) map[string]bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	out := map[string]bool{}
	for _, req := range fake.destroyRequests {
		out[req.Id] = true
	}
	return out
}

func waitForDestroy(t *testing.T, fake *fakeDispatchServer, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if destroyedNames(fake)[name] {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner %q was not destroyed within deadline (destroyed: %v)", name, destroyedNames(fake))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleCompleted_ReassignedRunner_ProductionScenario replays the
// production race end to end:
//
//	dispatch runner R for job A, runner S for job B
//	GitHub swaps them: in_progress(B, R), in_progress(A, S)
//	completed(A, S)  → must destroy S, must NOT touch R (mid-flight on B)
//	completed(B, R)  → must destroy R
func TestHandleCompleted_ReassignedRunner_ProductionScenario(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobA, jobB int64 = 101, 202
	runnerR := dispatchLinuxJob(t, s, prov, jobA)
	runnerS := dispatchLinuxJob(t, s, prov, jobB)
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}
	keyB := jobKey{Provider: prov.Name(), JobID: jobB}

	// GitHub swaps the assignments.
	s.handleInProgress(providers.JobEvent{
		Provider: prov, Action: "in_progress", Repo: "myrepo", JobID: jobB, RunnerName: runnerR,
	})
	s.handleInProgress(providers.JobEvent{
		Provider: prov, Action: "in_progress", Repo: "myrepo", JobID: jobA, RunnerName: runnerS,
	})

	s.mu.Lock()
	rbR, okR := s.runners[runnerR]
	rbS, okS := s.runners[runnerS]
	s.mu.Unlock()
	if !okR || !rbR.bound || rbR.boundKey != keyB {
		t.Fatalf("runner R binding = %+v, want bound to job B", rbR)
	}
	if !okS || !rbS.bound || rbS.boundKey != keyA {
		t.Fatalf("runner S binding = %+v, want bound to job A", rbS)
	}

	// Job A completes — on runner S. The old code destroyed
	// job.dispatched (= R) here, killing job B mid-flight.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobA, RunnerName: runnerS, Conclusion: "success",
	})

	waitForDestroy(t, fake, runnerS)
	if destroyedNames(fake)[runnerR] {
		t.Fatalf("runner R was destroyed on job A's completion — the reassignment race regressed")
	}

	// R's bookkeeping (filed under its dispatch intent, job A) must survive.
	s.mu.Lock()
	rj, rStillTracked := s.running[keyA]
	_, ledgerHasR := s.runners[runnerR]
	_, bStillTracked := s.running[keyB]
	s.mu.Unlock()
	if !rStillTracked || rj.dispatched != runnerR {
		t.Fatalf("runner R's entry (under intent key A) should survive job A's completion")
	}
	if !ledgerHasR {
		t.Fatal("runner R's ledger entry should survive job A's completion")
	}
	if bStillTracked {
		t.Fatal("runner S's entry (under intent key B) should be removed once S is destroyed")
	}

	// Job B completes — on runner R. Now R goes down.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobB, RunnerName: runnerR, Conclusion: "success",
	})

	waitForDestroy(t, fake, runnerR)

	s.mu.Lock()
	nRunning, nLedger := len(s.running), len(s.runners)
	s.mu.Unlock()
	if nRunning != 0 {
		t.Errorf("running map has %d entries after both completions, want 0", nRunning)
	}
	if nLedger != 0 {
		t.Errorf("runner ledger has %d entries after both completions, want 0", nLedger)
	}

	close(fake.waitBlock)
}

// TestHandleCompleted_NoReassignment_HappyPath pins that the common case
// (runner runs exactly the job it was dispatched for) behaves as before.
func TestHandleCompleted_NoReassignment_HappyPath(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobA int64 = 303
	runnerR := dispatchLinuxJob(t, s, prov, jobA)

	s.handleInProgress(providers.JobEvent{
		Provider: prov, Action: "in_progress", Repo: "myrepo", JobID: jobA, RunnerName: runnerR,
	})
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobA, RunnerName: runnerR, Conclusion: "success",
	})

	waitForDestroy(t, fake, runnerR)

	s.mu.Lock()
	nRunning, nLedger := len(s.running), len(s.runners)
	s.mu.Unlock()
	if nRunning != 0 {
		t.Errorf("running map has %d entries, want 0", nRunning)
	}
	if nLedger != 0 {
		t.Errorf("runner ledger has %d entries, want 0", nLedger)
	}

	close(fake.waitBlock)
}

// TestHandleCompleted_CancelledBeforeAssignment pins the fallback: a
// completed event with no runner name (job cancelled before any runner
// picked it up) still tears down the dispatch-intent runner, as long as
// that runner was never observed running a different job.
func TestHandleCompleted_CancelledBeforeAssignment(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobA int64 = 404
	runnerR := dispatchLinuxJob(t, s, prov, jobA)

	// No in_progress ever arrives; the job is cancelled unassigned.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobA, RunnerName: "", Conclusion: "cancelled",
	})

	waitForDestroy(t, fake, runnerR)

	s.mu.Lock()
	nRunning := len(s.running)
	s.mu.Unlock()
	if nRunning != 0 {
		t.Errorf("running map has %d entries, want 0", nRunning)
	}

	close(fake.waitBlock)
}

// TestHandleCompleted_EmptyRunnerName_IntentRunnerBoundElsewhere pins
// the guard on the fallback: when the completed event carries no runner
// name but the dispatch-intent runner is known to be running a DIFFERENT
// job, it must be left alone.
func TestHandleCompleted_EmptyRunnerName_IntentRunnerBoundElsewhere(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobA, jobB int64 = 505, 606
	runnerR := dispatchLinuxJob(t, s, prov, jobA)
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}

	// GitHub gives job B to runner R (dispatched for A).
	s.handleInProgress(providers.JobEvent{
		Provider: prov, Action: "in_progress", Repo: "myrepo", JobID: jobB, RunnerName: runnerR,
	})

	// Job A finishes with no runner name (e.g. cancelled while queued
	// after its intended runner was stolen).
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobA, RunnerName: "", Conclusion: "cancelled",
	})

	// R must not be destroyed and its bookkeeping must survive: job B's
	// completion is what tears it down.
	time.Sleep(50 * time.Millisecond)
	if destroyedNames(fake)[runnerR] {
		t.Fatal("runner R destroyed by empty-runner-name fallback while bound to job B")
	}
	s.mu.Lock()
	_, tracked := s.running[keyA]
	s.mu.Unlock()
	if !tracked {
		t.Fatal("runner R's entry should survive job A's runnerless completion")
	}

	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobB, RunnerName: runnerR, Conclusion: "success",
	})
	waitForDestroy(t, fake, runnerR)

	close(fake.waitBlock)
}

// TestHandleCompleted_ForeignRunner pins that a completed event naming a
// runner ephemerd does not own destroys nothing: the job ran elsewhere
// (peer daemon / GitHub-hosted), and our dispatch-intent runner stays up
// for another assignment or the orphan sweep.
func TestHandleCompleted_ForeignRunner(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   4,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobA int64 = 707
	runnerR := dispatchLinuxJob(t, s, prov, jobA)
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}

	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo",
		JobID: jobA, RunnerName: "somebody-elses-runner", Conclusion: "success",
	})

	time.Sleep(50 * time.Millisecond)
	if len(destroyedNames(fake)) != 0 {
		t.Fatalf("nothing should be destroyed for a foreign runner, got %v", destroyedNames(fake))
	}
	s.mu.Lock()
	_, tracked := s.running[keyA]
	_, ledger := s.runners[runnerR]
	s.mu.Unlock()
	if !tracked || !ledger {
		t.Fatal("dispatch-intent runner bookkeeping should survive a foreign-runner completion")
	}

	close(fake.waitBlock)
}

// TestHandleInProgress_UnknownOrEmptyRunner pins the no-op paths.
func TestHandleInProgress_UnknownOrEmptyRunner(t *testing.T) {
	s := New(Config{Log: quietLogger()})

	// Empty runner name: nothing to record.
	s.handleInProgress(providers.JobEvent{Action: "in_progress", Repo: "r", JobID: 1})
	// Runner we never dispatched: nothing to record.
	s.handleInProgress(providers.JobEvent{Action: "in_progress", Repo: "r", JobID: 1, RunnerName: "not-ours"})

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runners) != 0 {
		t.Errorf("runner ledger has %d entries, want 0", len(s.runners))
	}
}

// --- orphan sweep ---

// seedDispatchedRunner inserts a running entry + ledger entry directly,
// the way trackRunning would, with a controllable dispatch timestamp.
func seedDispatchedRunner(s *Scheduler, prov providers.Provider, jobID int64, name string, dispatchedAt time.Time, bound, observable bool) jobKey {
	key := jobKey{Provider: prov.Name(), JobID: jobID}
	_, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.running[key] = &runningJob{
		provider:   prov,
		claim:      &providers.Claim{RunnerID: jobID * 10, RunnerName: name, Repo: "myrepo"},
		repo:       "myrepo",
		cancel:     cancel,
		dispatched: name,
		startedAt:  dispatchedAt,
	}
	s.runners[name] = &runnerBinding{
		intentKey:    key,
		dispatchedAt: dispatchedAt,
		bound:        bound,
		observable:   observable,
	}
	if bound {
		s.runners[name].boundKey = jobKey{Provider: prov.Name(), JobID: jobID + 1000}
	}
	s.mu.Unlock()
	return key
}

// TestSweepOrphanRunners_GraceWindow pins the sweep matrix: only
// runners that are (a) past the grace window, (b) never bound, and
// (c) dispatched via an assignment-reporting provider in webhook mode
// are destroyed and deregistered.
func TestSweepOrphanRunners_GraceWindow(t *testing.T) {
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

	old := time.Now().Add(-11 * time.Minute)
	fresh := time.Now().Add(-1 * time.Minute)

	orphanKey := seedDispatchedRunner(s, prov, 1, "runner-orphan", old, false, true)
	boundKey := seedDispatchedRunner(s, prov, 2, "runner-bound", old, true, true)
	freshKey := seedDispatchedRunner(s, prov, 3, "runner-fresh", fresh, false, true)
	pollKey := seedDispatchedRunner(s, prov, 4, "runner-unobservable", old, false, false)

	s.sweepOrphanRunners()

	waitForDestroy(t, fake, "runner-orphan")
	got := destroyedNames(fake)
	for _, name := range []string{"runner-bound", "runner-fresh", "runner-unobservable"} {
		if got[name] {
			t.Errorf("sweep destroyed %q, which should have been exempt", name)
		}
	}

	s.mu.Lock()
	_, orphanTracked := s.running[orphanKey]
	_, boundTracked := s.running[boundKey]
	_, freshTracked := s.running[freshKey]
	_, pollTracked := s.running[pollKey]
	s.mu.Unlock()
	if orphanTracked {
		t.Error("orphaned runner should be removed from running")
	}
	if !boundTracked || !freshTracked || !pollTracked {
		t.Error("exempt runners should stay tracked")
	}

	// The orphan never ran a job, so it must be deregistered from the
	// provider (JIT runners only auto-remove after running a job).
	if rel := base.releases.Load(); rel != 1 {
		t.Errorf("ReleaseJob called %d times, want 1 (the orphan)", rel)
	}
}

// TestSweepOrphanRunners_DisabledOrPolling pins that the sweep is inert
// when disabled and when in polling mode (no in_progress events means
// "never bound" carries no information).
func TestSweepOrphanRunners_DisabledOrPolling(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		webhookMode bool
	}{
		{"disabled", false, true},
		{"polling mode", true, false},
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
				OrphanSweep:     OrphanSweepConfig{Enabled: tt.enabled, Grace: 10 * time.Minute},
				Log:             quietLogger(),
			})
			s.webhookMode = tt.webhookMode

			key := seedDispatchedRunner(s, prov, 1, "runner-x", time.Now().Add(-1*time.Hour), false, true)

			s.sweepOrphanRunners()
			time.Sleep(50 * time.Millisecond)

			if len(destroyedNames(fake)) != 0 {
				t.Errorf("sweep destroyed runners while %s: %v", tt.name, destroyedNames(fake))
			}
			s.mu.Lock()
			_, tracked := s.running[key]
			s.mu.Unlock()
			if !tracked {
				t.Error("runner should stay tracked")
			}
		})
	}
}
