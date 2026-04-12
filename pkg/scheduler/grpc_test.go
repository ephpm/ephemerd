package scheduler

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- toProto tests ---

func TestToProto_BasicFields(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	started := time.Now().Add(-5 * time.Minute)
	rj := &runningJob{
		repo:      "my-repo",
		image:     "ghcr.io/myorg/runner:latest",
		runnerID:  42,
		startedAt: started,
	}

	job := cs.toProto(123, rj)

	if job.Id != 123 {
		t.Errorf("Id = %d, want 123", job.Id)
	}
	if job.Repo != "my-repo" {
		t.Errorf("Repo = %q, want %q", job.Repo, "my-repo")
	}
	if job.Image != "ghcr.io/myorg/runner:latest" {
		t.Errorf("Image = %q", job.Image)
	}
	if job.RunnerId != 42 {
		t.Errorf("RunnerId = %d, want 42", job.RunnerId)
	}
	if job.Status != "running" {
		t.Errorf("Status = %q, want %q", job.Status, "running")
	}
	if job.StartedAt == "" {
		t.Error("StartedAt should not be empty")
	}
	if job.Uptime == "" {
		t.Error("Uptime should not be empty")
	}
}

func TestToProto_DispatchedJob(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	rj := &runningJob{
		repo:       "repo",
		dispatched: "ephemerd-repo-bold_tesla",
		startedAt:  time.Now(),
	}

	job := cs.toProto(456, rj)

	if job.Name != "ephemerd-repo-bold_tesla" {
		t.Errorf("Name = %q, want dispatched name", job.Name)
	}
}

func TestToProto_NilEnvAndNoDispatch(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	rj := &runningJob{
		repo:      "repo",
		startedAt: time.Now(),
	}

	job := cs.toProto(789, rj)

	// No env and no dispatched — Name should be empty
	if job.Name != "" {
		t.Errorf("Name = %q, want empty for non-dispatched job with nil env", job.Name)
	}
}

func TestToProto_UptimeIncreases(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	rj := &runningJob{
		repo:      "repo",
		startedAt: time.Now().Add(-1 * time.Hour),
	}

	job := cs.toProto(1, rj)

	// Uptime should be approximately 1 hour
	if job.Uptime == "0s" {
		t.Error("Uptime should not be 0s for job started 1h ago")
	}
}
