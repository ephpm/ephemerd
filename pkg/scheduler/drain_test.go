package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// These tests pin the graceful-drain contract: a SIGTERM (run-context
// cancellation) must NOT kill in-flight jobs. Job contexts hang off the
// detached jobsCtx (see bindContexts) and only die when drain() finishes
// waiting — either because the jobs completed or because ShutdownTimeout
// forced the kill. The older drain tests in dedup_test.go only checked the
// draining flag, which passed even when the signal was killing every job.

// --- job contexts survive run-context (signal) cancellation ---

func TestJobContext_SurvivesRunContextCancel(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	runCtx, cancelRun := context.WithCancel(context.Background())
	s.bindContexts(runCtx)

	jobCtx, cancelJob := s.jobContext()
	defer cancelJob()

	// Simulate SIGTERM: the signal-scoped context is canceled.
	cancelRun()

	select {
	case <-jobCtx.Done():
		t.Fatal("job context canceled by run-context cancellation; SIGTERM would kill running jobs and drain degenerates to kill")
	case <-time.After(50 * time.Millisecond):
	}

	// The drain-side cancel DOES propagate.
	s.jobsCancel()
	select {
	case <-jobCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("job context not canceled by jobsCtx cancel; force-kill at ShutdownTimeout would be a no-op")
	}
}

// --- job context honors JobTimeout independently of the run context ---

func TestJobContext_JobTimeoutStillApplies(t *testing.T) {
	s := New(Config{Log: quietLogger(), JobTimeout: 30 * time.Millisecond})
	s.bindContexts(context.Background())

	jobCtx, cancel := s.jobContext()
	defer cancel()

	select {
	case <-jobCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("job context did not expire at JobTimeout")
	}
	if jobCtx.Err() != context.DeadlineExceeded {
		t.Errorf("jobCtx.Err() = %v, want DeadlineExceeded", jobCtx.Err())
	}
}

// --- drain waits for a running job instead of killing it ---

func TestDrain_WaitsForRunningJob(t *testing.T) {
	s := New(Config{Log: quietLogger(), ShutdownTimeout: 10 * time.Second})
	runCtx, cancelRun := context.WithCancel(context.Background())
	s.bindContexts(runCtx)

	jobCtx, cancel := s.jobContext()
	key := jobKey{Provider: "github", JobID: 1}
	s.running[key] = &runningJob{repo: "r", cancel: cancel, startedAt: time.Now()}

	// The signal arrives...
	cancelRun()

	// ...and the job finishes on its own 150ms later, as a wait-goroutine
	// would report. Record whether its context was still alive then.
	canceledEarly := make(chan bool, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		canceledEarly <- jobCtx.Err() != nil
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		s.drain()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after the job finished")
	}
	if <-canceledEarly {
		t.Error("job context was already canceled while drain was waiting — the job was killed, not drained")
	}
	if !s.draining {
		t.Error("draining should be true after drain()")
	}
}

// --- drain force-kills at ShutdownTimeout ---

func TestDrain_ForceKillsAfterTimeout(t *testing.T) {
	s := New(Config{Log: quietLogger(), ShutdownTimeout: 200 * time.Millisecond})
	s.bindContexts(context.Background())

	jobCtx, cancel := s.jobContext()
	key := jobKey{Provider: "github", JobID: 1}
	s.running[key] = &runningJob{repo: "r", cancel: cancel, startedAt: time.Now()}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.drain()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after ShutdownTimeout")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("drain returned after %v, before ShutdownTimeout elapsed", elapsed)
	}

	s.mu.Lock()
	remaining := len(s.running)
	s.mu.Unlock()
	if remaining != 0 {
		t.Errorf("running map should be empty after force-kill, got %d entries", remaining)
	}
	select {
	case <-jobCtx.Done():
	case <-time.After(time.Second):
		t.Error("job context should be canceled after the force-kill at ShutdownTimeout")
	}
}

// --- Cordon rejects new claims, running jobs untouched ---

func TestCordon_RejectsNewClaims(t *testing.T) {
	mp := newMockProvider("github")
	s := New(Config{Providers: []providers.Provider{mp}, Log: quietLogger()})
	s.bindContexts(context.Background())

	// A running job that must survive the cordon.
	jobCtx, cancel := s.jobContext()
	defer cancel()
	runningKey := jobKey{Provider: "github", JobID: 1}
	s.running[runningKey] = &runningJob{repo: "r", cancel: cancel, startedAt: time.Now()}

	if got := s.Cordon(); got != 1 {
		t.Errorf("Cordon() = %d active jobs, want 1", got)
	}

	event := providers.JobEvent{Provider: mp, Action: "queued", Repo: "r", JobID: 2}
	s.handleQueued(context.Background(), event)

	mp.mu.Lock()
	claims := len(mp.claims)
	mp.mu.Unlock()
	if claims != 0 {
		t.Errorf("cordoned scheduler claimed %d job(s), want 0", claims)
	}
	if jobCtx.Err() != nil {
		t.Error("cordon must not cancel running job contexts")
	}
	s.mu.Lock()
	_, stillRunning := s.running[runningKey]
	s.mu.Unlock()
	if !stillRunning {
		t.Error("cordon must not remove running jobs")
	}
}

// --- Uncordon restores claiming ---

func TestUncordon_RestoresClaiming(t *testing.T) {
	// claimErrorProvider lets handleQueued run all the way to ClaimJob
	// (proving the draining gate is open) without touching the nil Runtime:
	// the claim error aborts provisioning right after.
	p := newClaimErrorProvider("github", errors.New("claim rejected by test"))
	s := New(Config{Providers: []providers.Provider{p}, Log: quietLogger()})
	s.bindContexts(context.Background())

	s.Cordon()
	s.handleQueued(context.Background(), providers.JobEvent{Provider: p, Action: "queued", Repo: "r", JobID: 5})
	if got := p.claims.Load(); got != 0 {
		t.Fatalf("cordoned scheduler claimed %d job(s), want 0", got)
	}

	s.Uncordon()
	s.mu.Lock()
	draining := s.draining
	// The seen stamp from the rejected pass would dedup the retry; expire
	// it the way a poll after seenTTL would observe it.
	delete(s.seen, jobKey{Provider: "github", JobID: 5})
	s.mu.Unlock()
	if draining {
		t.Fatal("Uncordon() should clear the draining flag")
	}

	s.handleQueued(context.Background(), providers.JobEvent{Provider: p, Action: "queued", Repo: "r", JobID: 5})
	if got := p.claims.Load(); got == 0 {
		t.Error("uncordoned scheduler never attempted a claim; claiming was not restored")
	}
}
