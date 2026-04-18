package github

import (
	"log/slog"
	"testing"

	gh "github.com/google/go-github/v72/github"

	ghclient "github.com/ephpm/ephemerd/pkg/github"
)

func TestName(t *testing.T) {
	p := New(nil, slog.Default())
	if p.Name() != "github" {
		t.Errorf("Name() = %q, want %q", p.Name(), "github")
	}
}

func TestDefaultImage(t *testing.T) {
	p := New(nil, slog.Default())
	want := "ghcr.io/actions/actions-runner:latest"
	if p.DefaultImage() != want {
		t.Errorf("DefaultImage() = %q, want %q", p.DefaultImage(), want)
	}
}

func TestDefaultJobImage_Empty(t *testing.T) {
	p := New(nil, slog.Default())
	if p.DefaultJobImage() != "" {
		t.Errorf("DefaultJobImage() = %q, want empty", p.DefaultJobImage())
	}
}

func TestConvertEvent_FullJob(t *testing.T) {
	id := int64(123)
	runID := int64(456)
	conclusion := "success"
	job := &gh.WorkflowJob{
		ID:         &id,
		RunID:      &runID,
		Conclusion: &conclusion,
		Labels:     []string{"self-hosted", "linux", "x64"},
	}

	ev := convertEvent(ghclient.JobEvent{
		Action: "queued",
		Repo:   "myorg/myrepo",
		Job:    job,
	})

	if ev.Action != "queued" {
		t.Errorf("Action = %q, want %q", ev.Action, "queued")
	}
	if ev.Repo != "myorg/myrepo" {
		t.Errorf("Repo = %q, want %q", ev.Repo, "myorg/myrepo")
	}
	if ev.JobID != 123 {
		t.Errorf("JobID = %d, want 123", ev.JobID)
	}
	if ev.RunID != 456 {
		t.Errorf("RunID = %d, want 456", ev.RunID)
	}
	if ev.Conclusion != "success" {
		t.Errorf("Conclusion = %q, want %q", ev.Conclusion, "success")
	}
	if len(ev.Labels) != 3 {
		t.Fatalf("Labels len = %d, want 3", len(ev.Labels))
	}
	if ev.Labels[0] != "self-hosted" {
		t.Errorf("Labels[0] = %q, want %q", ev.Labels[0], "self-hosted")
	}
	if ev.Raw != job {
		t.Error("Raw should point to the original WorkflowJob")
	}
}

func TestConvertEvent_NilJob(t *testing.T) {
	ev := convertEvent(ghclient.JobEvent{
		Action: "completed",
		Repo:   "myorg/myrepo",
		Job:    nil,
	})

	if ev.Action != "completed" {
		t.Errorf("Action = %q, want %q", ev.Action, "completed")
	}
	if ev.Repo != "myorg/myrepo" {
		t.Errorf("Repo = %q, want %q", ev.Repo, "myorg/myrepo")
	}
	if ev.JobID != 0 {
		t.Errorf("JobID = %d, want 0", ev.JobID)
	}
	if ev.RunID != 0 {
		t.Errorf("RunID = %d, want 0", ev.RunID)
	}
	if ev.Conclusion != "" {
		t.Errorf("Conclusion = %q, want empty", ev.Conclusion)
	}
	if len(ev.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0", len(ev.Labels))
	}
	// ev.Job is (*gh.WorkflowJob)(nil), which becomes a non-nil interface
	// when stored in Raw (any). This is expected Go behavior.
	if ev.Raw.(*gh.WorkflowJob) != nil {
		t.Error("Raw should hold a nil *WorkflowJob when Job is nil")
	}
}

func TestConvertEvent_EmptyLabels(t *testing.T) {
	id := int64(1)
	job := &gh.WorkflowJob{
		ID:     &id,
		Labels: nil,
	}

	ev := convertEvent(ghclient.JobEvent{
		Action: "queued",
		Repo:   "org/repo",
		Job:    job,
	})

	if len(ev.Labels) != 0 {
		t.Errorf("Labels len = %d, want 0 for nil labels", len(ev.Labels))
	}
}
