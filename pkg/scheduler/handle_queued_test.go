package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// claimErrorProvider returns a configurable error from ClaimJob.
type claimErrorProvider struct {
	mockProvider
	err          error
	releaseCount atomic.Int32
}

func newClaimErrorProvider(name string, err error) *claimErrorProvider {
	return &claimErrorProvider{
		mockProvider: *newMockProvider(name),
		err:          err,
	}
}

func (p *claimErrorProvider) ClaimJob(_ context.Context, _ *providers.JobEvent, _ string, _ []string) (*providers.Claim, error) {
	return nil, p.err
}

func (p *claimErrorProvider) ReleaseJob(_ context.Context, _ *providers.Claim) error {
	p.releaseCount.Add(1)
	return nil
}

func (p *claimErrorProvider) FetchJobImage(_ context.Context, _ *providers.JobEvent) string {
	return ""
}

// TestHandleQueued_DrainNoClaim verifies a draining scheduler does not
// proceed past the draining check. The seen entry is recorded but no slot is
// acquired and the provider is never asked to claim.
func TestHandleQueued_DrainNoClaim(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers:     []providers.Provider{mp},
		MaxConcurrent: 1,
		Log:           testLogger(),
	})
	s.draining = true

	event := providers.JobEvent{
		Provider: mp,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    42,
	}

	s.handleQueued(context.Background(), event)

	if got := len(mp.claims); got != 0 {
		t.Errorf("expected 0 claims when draining, got %d", got)
	}
	if got := len(s.running); got != 0 {
		t.Errorf("expected 0 running jobs when draining, got %d", got)
	}
}

// TestHandleQueued_SkipsMacOSWithoutVMConfig pins the macOS deferral path:
// when MacOSVMConfig is nil but the job has macOS labels, the scheduler must
// remove the seen entry so the next poll retries. The provider is never asked
// to claim.
func TestHandleQueued_SkipsMacOSWithoutVMOrNativeConfig(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{mp},
		Log:       testLogger(),
		// No MacOSVMConfig and no MacOSModeForRepo — macOS jobs should be deferred
	})

	event := providers.JobEvent{
		Provider: mp,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    99,
		Labels:   []string{"self-hosted", "macos-14"},
	}

	s.handleQueued(context.Background(), event)

	if got := len(mp.claims); got != 0 {
		t.Errorf("macOS job without VM or native config should not claim, got %d claims", got)
	}
	s.mu.Lock()
	_, seen := s.seen[keyFor(event)]
	s.mu.Unlock()
	if seen {
		t.Error("macOS job without VM or native config should be unseen so it retries on next poll")
	}
}

// TestHandleQueued_DedupReentry verifies the second handleQueued call within
// the seenTTL window short-circuits before claiming.
func TestHandleQueued_DedupReentry(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{mp},
		Log:       testLogger(),
	})
	s.draining = true // make it exit before reaching ClaimJob

	event := providers.JobEvent{
		Provider: mp,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    7,
	}

	// First call: records in seen.
	s.handleQueued(context.Background(), event)
	s.mu.Lock()
	_, seen := s.seen[keyFor(event)]
	s.mu.Unlock()
	if !seen {
		t.Fatal("event not recorded in seen after first handleQueued")
	}

	// Second call: should hit the dedup branch (already seen).
	// We don't have a directly observable side-effect but at least confirm
	// no new claims/running entries were created.
	s.handleQueued(context.Background(), event)
	if got := len(mp.claims); got != 0 {
		t.Errorf("dedup should prevent claims, got %d", got)
	}
}

// TestHandleQueued_AlreadyRunningWithProvider verifies that if a job is
// already in s.running, handleQueued exits early without touching seen or
// sem and never asks the provider to claim.
func TestHandleQueued_AlreadyRunningWithProvider(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers:     []providers.Provider{mp},
		MaxConcurrent: 1,
		Log:           testLogger(),
	})

	event := providers.JobEvent{
		Provider: mp,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    11,
	}

	// Pre-populate running map.
	s.running[keyFor(event)] = &runningJob{repo: "myrepo", startedAt: time.Now()}

	s.handleQueued(context.Background(), event)

	if got := len(mp.claims); got != 0 {
		t.Errorf("already-running job should not claim, got %d claims", got)
	}
}

// TestHandleQueued_SkipsMismatchedLabels verifies canHandleJob rejection
// before any side-effects are recorded.
func TestHandleQueued_SkipsMismatchedLabels(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{mp},
		Log:       testLogger(),
	})

	// arch: pick the opposite of the host architecture so canHandleJob
	// definitively rejects regardless of which platform runs the test.
	wrongArch := "arm64"
	if expectedArchLabel() == "arm64" {
		wrongArch = "amd64"
	}

	event := providers.JobEvent{
		Provider: mp,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    21,
		Labels:   []string{"self-hosted", wrongArch},
	}

	s.handleQueued(context.Background(), event)

	if got := len(mp.claims); got != 0 {
		t.Errorf("mismatched-label job should not claim, got %d", got)
	}
	s.mu.Lock()
	_, seen := s.seen[keyFor(event)]
	s.mu.Unlock()
	if seen {
		t.Error("mismatched-label job should not be added to seen")
	}
}

// TestClaimJob_NonRetryableError pins that non-409 errors propagate without retry.
func TestClaimJob_NonRetryableError(t *testing.T) {
	wantErr := errors.New("permission denied")
	p := newClaimErrorProvider("github", wantErr)

	s := New(Config{
		Providers: []providers.Provider{p},
		Log:       testLogger(),
	})

	event := &providers.JobEvent{
		Provider: p,
		Repo:     "myrepo",
		JobID:    1,
	}
	_, err := s.claimJob(context.Background(), event, nil, testLogger(), 3)
	if err == nil {
		t.Fatal("expected error from claimJob")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("got %v, want %v", err, wantErr)
	}
}

// TestClaimJob_RetriableConflict_ExhaustsRetries pins that a 409-style error
// keeps retrying with a new name and ultimately returns the last error.
func TestClaimJob_RetriableConflict_ExhaustsRetries(t *testing.T) {
	conflictErr := errors.New("POST .../runners: 409 Conflict")
	p := newClaimErrorProvider("github", conflictErr)

	s := New(Config{
		Providers: []providers.Provider{p},
		Log:       testLogger(),
	})

	event := &providers.JobEvent{
		Provider: p,
		Repo:     "myrepo",
		JobID:    1,
	}
	_, err := s.claimJob(context.Background(), event, nil, testLogger(), 3)
	if err == nil {
		t.Fatal("expected error from claimJob")
	}
	if err.Error() != conflictErr.Error() {
		t.Errorf("err = %v, want %v", err, conflictErr)
	}
}

// TestClaimJob_ZeroRetries verifies maxRetries=0 returns nil claim and nil error
// (no attempt made, no lastErr accumulated).
func TestClaimJob_ZeroRetries(t *testing.T) {
	p := newClaimErrorProvider("github", errors.New("x"))

	s := New(Config{
		Providers: []providers.Provider{p},
		Log:       testLogger(),
	})

	event := &providers.JobEvent{Provider: p, Repo: "r", JobID: 1}
	claim, err := s.claimJob(context.Background(), event, nil, testLogger(), 0)
	if err != nil {
		t.Errorf("zero retries: err = %v, want nil", err)
	}
	if claim != nil {
		t.Errorf("zero retries: claim = %+v, want nil", claim)
	}
}

// TestHandleCompleted_NotInRunning_NoOp pins that completed events for jobs
// not in the running map are a no-op (no panic, no provider calls).
func TestHandleCompleted_NotInRunning_NoOp(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{mp},
		Log:       testLogger(),
	})

	// No running job — handleCompleted should just return.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider:   mp,
		Action:     "completed",
		Repo:       "myrepo",
		JobID:      999,
		Conclusion: "success",
	})

	if got := len(mp.releases); got != 0 {
		t.Errorf("expected 0 releases for unknown job, got %d", got)
	}
}

// TestHandleCompleted_DispatchedJob_NilDispatcher pins that handleCompleted
// for a dispatched job works even if the dispatcher field is nil (graceful
// no-op of the dispatcher destroy branch).
func TestHandleCompleted_DispatchedJob_NilDispatcher(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{
		Providers: []providers.Provider{mp},
		Log:       testLogger(),
	})
	// LinuxDispatcher is intentionally nil even though dispatched != "".

	_, cancel := context.WithCancel(context.Background())

	key := jobKey{Provider: "github", JobID: 1}
	s.running[key] = &runningJob{
		provider:   mp,
		claim:      &providers.Claim{RunnerID: 10, Repo: "r"},
		repo:       "r",
		dispatched: "ephemerd-x",
		cancel:     cancel,
		startedAt:  time.Now(),
	}

	// Should not panic — we exercise the dispatched branch's nil-dispatcher
	// guard.
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider:   mp,
		Action:     "completed",
		Repo:       "r",
		JobID:      1,
		Conclusion: "success",
	})

	s.mu.Lock()
	_, stillRunning := s.running[key]
	s.mu.Unlock()
	if stillRunning {
		t.Error("job should be removed from running map after completion")
	}
}

// TestBackoffDuration_AfterSuccessReset verifies that after a successful
// claim path resets the backoff, the next failure starts at 2s again.
func TestBackoffDuration_AfterSuccessReset(t *testing.T) {
	repo := "test-reset-after-success"
	resetBackoff(repo)

	// Build up backoff.
	if d := backoffDuration(repo); d != 2*time.Second {
		t.Fatalf("first backoff = %v, want 2s", d)
	}
	if d := backoffDuration(repo); d != 4*time.Second {
		t.Fatalf("second backoff = %v, want 4s", d)
	}

	// Reset (this is what handleCompleted does on success).
	resetBackoff(repo)

	if d := backoffDuration(repo); d != 2*time.Second {
		t.Errorf("after reset, first backoff = %v, want 2s", d)
	}
	resetBackoff(repo)
}

// TestHandleQueued_ZombieSkip verifies that a job which keeps reaching
// provisioning but never runs to completion (GitHub lists it queued forever)
// is skipped after maxProvisionAttempts, instead of re-provisioning on every
// poll. draining=true keeps any real provisioning from happening — the zombie
// check runs before the drain check, so the attempt counter still advances.
func TestHandleQueued_ZombieSkip(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{Providers: []providers.Provider{mp}, Log: testLogger()})
	s.draining = true

	event := providers.JobEvent{Provider: mp, Action: "queued", Repo: "myrepo", JobID: 7}
	key := keyFor(event)

	// Simulate the seenTTL gap between polls by clearing the dedup entries so
	// each call is treated as a fresh provisioning pass.
	for i := 0; i < maxProvisionAttempts+3; i++ {
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.pending, key)
		s.mu.Unlock()
		s.handleQueued(context.Background(), event)
	}

	s.mu.Lock()
	attempts := s.attempts[key]
	s.mu.Unlock()

	// The counter keeps climbing past the cap (each poll still increments).
	if attempts <= maxProvisionAttempts {
		t.Errorf("attempts = %d, want > %d (cap should be exceeded)", attempts, maxProvisionAttempts)
	}
	// A zombie is never claimed.
	if got := len(mp.claims); got != 0 {
		t.Errorf("zombie job should never be claimed, got %d claims", got)
	}
}

// TestCleanSeen_PrunesAttempts verifies the zombie counter is reset once a
// job stops appearing in the queue (its seen entry expires), so a later
// legitimate rerun of the same job id starts fresh.
func TestCleanSeen_PrunesAttempts(t *testing.T) {
	s := New(Config{Log: testLogger()})
	key := jobKey{Provider: "github", JobID: 99}

	s.mu.Lock()
	s.seen[key] = time.Now().Add(-seenTTL - time.Minute) // expired
	s.attempts[key] = maxProvisionAttempts + 2
	s.mu.Unlock()

	s.cleanSeen()

	s.mu.Lock()
	_, seenExists := s.seen[key]
	_, attemptsExist := s.attempts[key]
	s.mu.Unlock()

	if seenExists {
		t.Error("expired seen entry should be pruned")
	}
	if attemptsExist {
		t.Error("attempts counter should be pruned alongside the seen entry")
	}
}
