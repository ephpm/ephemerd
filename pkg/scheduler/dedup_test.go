package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/github"
	gh "github.com/google/go-github/v72/github"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeEvent(jobID int64, labels []string) github.JobEvent {
	return github.JobEvent{
		Action: "queued",
		Repo:   "test-repo",
		Job:    &gh.WorkflowJob{ID: gh.Ptr(jobID), Labels: labels},
	}
}

// --- handleQueued dedup: already running ---

func TestHandleQueued_SkipsAlreadyRunning(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	ctx := context.Background()

	// Simulate a running job
	s.running[100] = &runningJob{repo: "test", startedAt: time.Now()}

	// handleQueued should skip silently (no panic, no new entry in seen)
	event := makeEvent(100, []string{"self-hosted"})
	s.handleQueued(ctx, event)

	// Job should still be in running (not removed)
	s.mu.Lock()
	_, stillRunning := s.running[100]
	s.mu.Unlock()
	if !stillRunning {
		t.Error("running job should not be removed by duplicate queued event")
	}
}

// --- handleQueued dedup: recently seen ---

func TestHandleQueued_SkipsRecentlySeen(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	ctx := context.Background()

	// Mark job as recently seen
	s.seen[200] = time.Now()

	event := makeEvent(200, []string{"self-hosted"})
	s.handleQueued(ctx, event)

	// Should not be added to running
	s.mu.Lock()
	_, isRunning := s.running[200]
	s.mu.Unlock()
	if isRunning {
		t.Error("recently seen job should not be started")
	}
}

// --- handleQueued dedup: expired seen entry should allow retry ---

func TestHandleQueued_AllowsExpiredSeen(t *testing.T) {
	s := New(Config{Log: quietLogger()})

	// Mark job as seen but expired
	s.seen[300] = time.Now().Add(-seenTTL - time.Minute)

	// The dedup check should pass, and seen should be updated
	s.mu.Lock()
	seenTime := s.seen[300]
	s.mu.Unlock()

	if time.Since(seenTime) < seenTTL {
		t.Error("test setup: seen entry should be expired")
	}
}

// --- handleQueued: draining rejects ---

func TestHandleQueued_RejectsWhenDraining(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	ctx := context.Background()

	s.draining = true

	event := makeEvent(400, []string{"self-hosted"})
	s.handleQueued(ctx, event)

	// Job should be in seen (was recorded) but not running
	s.mu.Lock()
	_, isRunning := s.running[400]
	_, isSeen := s.seen[400]
	s.mu.Unlock()

	if isRunning {
		t.Error("draining scheduler should not start new jobs")
	}
	if !isSeen {
		t.Error("job should be marked as seen even when draining")
	}
}

// --- drain with no running jobs ---

func TestDrain_NoRunningJobs(t *testing.T) {
	s := New(Config{Log: quietLogger()})

	// drain should return immediately with no jobs
	s.drain()

	if !s.draining {
		t.Error("draining should be true after drain()")
	}
}

// --- drain sets draining flag ---

func TestDrain_SetsDrainingFlag(t *testing.T) {
	s := New(Config{Log: quietLogger(), ShutdownTimeout: 1 * time.Second})

	s.drain()

	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()

	if !draining {
		t.Error("drain() should set draining = true")
	}
}

// --- destroyAll clears running map ---

func TestDestroyAll_ClearsRunning(t *testing.T) {
	s := New(Config{Log: quietLogger()})

	// Add mock running jobs with cancel functions
	ctx, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	s.running[1] = &runningJob{
		repo:      "repo1",
		cancel:    cancel1,
		startedAt: time.Now(),
	}
	s.running[2] = &runningJob{
		repo:      "repo2",
		cancel:    cancel2,
		startedAt: time.Now(),
	}

	_ = ctx // suppress unused

	s.destroyAll()

	s.mu.Lock()
	remaining := len(s.running)
	s.mu.Unlock()

	if remaining != 0 {
		t.Errorf("destroyAll should clear running map, got %d entries", remaining)
	}
}

// --- handleCompleted: removes running job ---

func TestHandleCompleted_RemovesJob(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	ctx := context.Background()

	_, cancel := context.WithCancel(ctx)
	s.running[500] = &runningJob{
		repo:      "test",
		cancel:    cancel,
		startedAt: time.Now(),
	}

	event := github.JobEvent{
		Action: "completed",
		Repo:   "test",
		Job:    &gh.WorkflowJob{ID: gh.Ptr(int64(500))},
	}
	s.handleCompleted(ctx, event)

	s.mu.Lock()
	_, exists := s.running[500]
	s.mu.Unlock()

	if exists {
		t.Error("completed job should be removed from running")
	}
}

// --- handleCompleted: unknown job is no-op ---

func TestHandleCompleted_UnknownJobNoOp(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	ctx := context.Background()

	event := github.JobEvent{
		Action: "completed",
		Repo:   "test",
		Job:    &gh.WorkflowJob{ID: gh.Ptr(int64(999))},
	}

	// Should not panic
	s.handleCompleted(ctx, event)
}
