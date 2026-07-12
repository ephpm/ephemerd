package scheduler

// Tests for the event-driven stranded-job self-heal (reprovisionIfStranded).
//
// GitHub treats same-label JIT runners as fungible: the runner ephemerd
// dispatches "for" job A routinely runs a sibling job B instead. When that
// runner exits (having run B) job A may still be queued with no runner. Because
// handleQueued marks A "seen" on the QUEUED event, nothing re-provisions it and
// it strands. The fix reacts to the runner-exit event we already have: if the
// dispatched job was never OBSERVED going in_progress/completed (started[key]
// unset), it never ran, so we clear its dedup and re-dispatch it — no polling.

import (
	"context"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// linuxQueuedEvent builds a queued Linux job event for the given provider.
func linuxQueuedEvent(prov providers.Provider, jobID int64) providers.JobEvent {
	return providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    jobID,
		Labels:   []string{"self-hosted", "linux"},
	}
}

// waitForClaims blocks until the provider's claim count reaches want (or the
// deadline elapses), then returns the observed count.
func waitForClaims(t *testing.T, prov *claimCountingProvider, want int32) int32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := prov.claims.Load()
		if got >= want {
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForRunning blocks until s.running contains key (or the deadline elapses).
func waitForRunning(t *testing.T, s *Scheduler, key jobKey) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, ok := s.running[key]
		s.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReprovisionIfStranded_ReprovisionsUnstartedJob is the core case: a runner
// was dispatched for job A, exited, and A was never observed running. The
// self-heal must re-dispatch A (a second claim, a fresh running entry).
func TestReprovisionIfStranded_ReprovisionsUnstartedJob(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   8,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobA int64 = 111
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}
	event := linuxQueuedEvent(prov, jobA)

	// First dispatch: one claim, one running entry (wait-goroutine blocks).
	s.handleQueued(context.Background(), event)
	if got := prov.claims.Load(); got != 1 {
		t.Fatalf("after first dispatch: claims = %d, want 1", got)
	}

	// Simulate the runner exiting without job A ever going in_progress: remove
	// the running entry the way the wait-goroutine's cleanup does.
	s.mu.Lock()
	rj := s.running[keyA]
	s.untrackRunningLocked(keyA, rj)
	s.mu.Unlock()

	// The event-driven self-heal fires (as it would at the end of the
	// wait-goroutine). started[keyA] is unset, so it re-dispatches.
	s.reprovisionIfStranded(context.Background(), event)

	if !waitForRunning(t, s, keyA) {
		t.Fatal("job A was not re-provisioned after its runner exited unstarted")
	}
	if got := waitForClaims(t, base, 2); got != 2 {
		t.Fatalf("after re-provision: claims = %d, want 2", got)
	}

	close(fake.waitBlock)
}

// TestReprovisionIfStranded_SkipsObservedJob pins that a job we DID observe
// running (in_progress fired) is satisfied and never re-provisioned, even
// though its dispatch-intent runner later exits.
func TestReprovisionIfStranded_SkipsObservedJob(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   8,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobA int64 = 222
	keyA := jobKey{Provider: prov.Name(), JobID: jobA}
	event := linuxQueuedEvent(prov, jobA)

	s.handleQueued(context.Background(), event)
	s.mu.Lock()
	runnerR := s.running[keyA].dispatched
	s.mu.Unlock()

	// Job A is OBSERVED running (on its own runner here; a sibling runner
	// would be equivalent — started is keyed on the job, not the runner).
	s.handleInProgress(providers.JobEvent{
		Provider: prov, Action: "in_progress", Repo: "myrepo", JobID: jobA, RunnerName: runnerR,
	})

	// Runner exits; cleanup removes the running entry.
	s.mu.Lock()
	rj := s.running[keyA]
	s.untrackRunningLocked(keyA, rj)
	s.mu.Unlock()

	s.reprovisionIfStranded(context.Background(), event)

	// Give any (erroneous) re-dispatch a chance to happen, then assert none did.
	time.Sleep(100 * time.Millisecond)
	if got := base.claims.Load(); got != 1 {
		t.Fatalf("observed job was re-provisioned: claims = %d, want 1", got)
	}
	s.mu.Lock()
	_, tracked := s.running[keyA]
	s.mu.Unlock()
	if tracked {
		t.Fatal("observed job should not have a new running entry")
	}

	close(fake.waitBlock)
}

// TestReprovisionIfStranded_SkipsCompletedJob pins that a job that reached a
// terminal state (completed, even cancelled-before-start with no in_progress)
// is never re-provisioned.
func TestReprovisionIfStranded_SkipsCompletedJob(t *testing.T) {
	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}
	s := New(Config{Providers: []providers.Provider{prov}, Log: quietLogger()})
	s.webhookMode = true

	const jobA int64 = 333
	event := linuxQueuedEvent(prov, jobA)

	// completed marks the job satisfied (records started) with no prior
	// in_progress — the cancelled-while-queued shape.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider: prov, Action: "completed", Repo: "myrepo", JobID: jobA, Conclusion: "cancelled",
	})

	s.reprovisionIfStranded(context.Background(), event)

	time.Sleep(100 * time.Millisecond)
	if got := base.claims.Load(); got != 0 {
		t.Fatalf("completed job was re-provisioned: claims = %d, want 0", got)
	}
}

// TestReprovisionIfStranded_Guards pins every short-circuit: nothing is
// re-dispatched when we are not in webhook mode, are draining, already have the
// job running/pending, or have exhausted the zombie provision cap.
func TestReprovisionIfStranded_Guards(t *testing.T) {
	const jobA int64 = 444

	tests := []struct {
		name  string
		setup func(s *Scheduler, key jobKey)
	}{
		{"not webhook mode", func(s *Scheduler, key jobKey) { s.webhookMode = false }},
		{"draining", func(s *Scheduler, key jobKey) { s.webhookMode = true; s.draining = true }},
		{"already running", func(s *Scheduler, key jobKey) {
			s.webhookMode = true
			s.running[key] = &runningJob{}
		}},
		{"already pending", func(s *Scheduler, key jobKey) {
			s.webhookMode = true
			s.pending[key] = struct{}{}
		}},
		{"over zombie cap", func(s *Scheduler, key jobKey) {
			s.webhookMode = true
			s.attempts[key] = maxProvisionAttempts + 1
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := newClaimCountingProvider("github")
			prov := &reportingProvider{claimCountingProvider: base}
			s := New(Config{Providers: []providers.Provider{prov}, Log: quietLogger()})

			key := jobKey{Provider: prov.Name(), JobID: jobA}
			tt.setup(s, key)

			s.reprovisionIfStranded(context.Background(), linuxQueuedEvent(prov, jobA))

			time.Sleep(50 * time.Millisecond)
			if got := base.claims.Load(); got != 0 {
				t.Fatalf("guard %q did not short-circuit: claims = %d, want 0", tt.name, got)
			}
		})
	}
}

// TestReprovisionIfStranded_WaitGoroutineWiringAndZombieCap proves two things
// end-to-end through the real Linux wait-goroutine: (1) reprovisionIfStranded
// is actually wired into runner exit, and (2) a job that keeps stranding
// (never observed running) converges on the zombie provision cap rather than
// re-provisioning forever. With Wait unblocked, each re-dispatched runner exits
// immediately and re-provisions again until attempts exceeds the cap.
func TestReprovisionIfStranded_WaitGoroutineWiringAndZombieCap(t *testing.T) {
	// Wait returns immediately (waitBlock starts closed) so each runner exits
	// as soon as it is dispatched, driving the strand->re-provision loop.
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	close(fake.waitBlock)
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	base := newClaimCountingProvider("github")
	prov := &reportingProvider{claimCountingProvider: base}

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   8,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobA int64 = 555

	s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobA))

	// The job is never observed running, so every runner exit re-provisions
	// until the zombie cap. handleQueued increments attempts and then bails
	// BEFORE claiming once attempts exceeds the cap, so the last successful
	// claim is the maxProvisionAttempts'th; the pass that pushes attempts to
	// cap+1 is a no-op claim-wise and ends the loop.
	wantClaims := int32(maxProvisionAttempts)
	got := waitForClaims(t, base, wantClaims)
	if got != wantClaims {
		t.Fatalf("claims = %d, want %d (strand->reprovision should stop at the zombie cap)", got, wantClaims)
	}

	// Give any (bug) extra re-provision a chance, then confirm it stayed capped.
	time.Sleep(150 * time.Millisecond)
	if got := base.claims.Load(); got != wantClaims {
		t.Fatalf("claims grew past the cap to %d, want %d", got, wantClaims)
	}

	// Once the loop stops, the last runner's exit leaves nothing tracked.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("running map has %d entries after the strand loop settled, want 0", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCleanSeen_PrunesStarted pins that the started (satisfied) set is pruned
// with a TTL that outlives JobTimeout, so a long-running sibling runner's late
// exit can't falsely re-provision an already-run job, while stale entries do
// eventually get reclaimed.
func TestCleanSeen_PrunesStarted(t *testing.T) {
	s := New(Config{JobTimeout: 30 * time.Minute, Log: quietLogger()})

	fresh := jobKey{Provider: "github", JobID: 1}
	old := jobKey{Provider: "github", JobID: 2}

	s.mu.Lock()
	// startedTTL = JobTimeout + seenTTL = 40m. "fresh" is well inside it;
	// "old" is well past it.
	s.started[fresh] = time.Now().Add(-20 * time.Minute)
	s.started[old] = time.Now().Add(-90 * time.Minute)
	s.mu.Unlock()

	s.cleanSeen()

	s.mu.Lock()
	_, freshKept := s.started[fresh]
	_, oldKept := s.started[old]
	s.mu.Unlock()

	if !freshKept {
		t.Error("started entry within the JobTimeout window was pruned too early")
	}
	if oldKept {
		t.Error("started entry well past the TTL was not pruned")
	}
}
