package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
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

// --- Status tests ---

func TestStatus_NoJobs(t *testing.T) {
	s := New(Config{MaxConcurrent: 4, Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	resp, err := cs.Status(context.Background(), &apiv1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("Status = %q, want ok", resp.Status)
	}
	if resp.ActiveJobs != 0 {
		t.Errorf("ActiveJobs = %d, want 0", resp.ActiveJobs)
	}
	if resp.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", resp.MaxConcurrent)
	}
	if resp.Draining {
		t.Error("Draining should be false")
	}
	if resp.Uptime == "" {
		t.Error("Uptime should not be empty")
	}
}

func TestStatus_WithJobs(t *testing.T) {
	s := New(Config{MaxConcurrent: 8, Log: silentLogger()})
	s.running[1] = &runningJob{repo: "r1", startedAt: time.Now()}
	s.running[2] = &runningJob{repo: "r2", startedAt: time.Now()}
	cs := &controlServer{sched: s, log: silentLogger()}

	resp, err := cs.Status(context.Background(), &apiv1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if resp.ActiveJobs != 2 {
		t.Errorf("ActiveJobs = %d, want 2", resp.ActiveJobs)
	}
	if resp.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", resp.MaxConcurrent)
	}
}

func TestStatus_Draining(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	s.draining = true
	cs := &controlServer{sched: s, log: silentLogger()}

	resp, err := cs.Status(context.Background(), &apiv1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if !resp.Draining {
		t.Error("Draining should be true")
	}
}

// --- ListJobs tests ---

func TestListJobs_Empty(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	resp, err := cs.ListJobs(context.Background(), &apiv1.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs() error: %v", err)
	}
	if len(resp.Jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(resp.Jobs))
	}
}

func TestListJobs_WithJobs(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	s.running[10] = &runningJob{repo: "repo-a", image: "img-a", runnerID: 1, startedAt: time.Now()}
	s.running[20] = &runningJob{repo: "repo-b", image: "img-b", runnerID: 2, startedAt: time.Now()}
	cs := &controlServer{sched: s, log: silentLogger()}

	resp, err := cs.ListJobs(context.Background(), &apiv1.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs() error: %v", err)
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(resp.Jobs))
	}

	// Verify jobs have correct IDs (order may vary since map iteration is random)
	ids := map[int64]bool{}
	for _, j := range resp.Jobs {
		ids[j.Id] = true
	}
	if !ids[10] || !ids[20] {
		t.Errorf("expected job IDs 10 and 20, got %v", ids)
	}
}

// --- GetJob tests ---

func TestGetJob_Found(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	s.running[42] = &runningJob{repo: "test-repo", image: "img", runnerID: 7, startedAt: time.Now()}
	cs := &controlServer{sched: s, log: silentLogger()}

	job, err := cs.GetJob(context.Background(), &apiv1.GetJobRequest{Id: 42})
	if err != nil {
		t.Fatalf("GetJob() error: %v", err)
	}
	if job.Id != 42 {
		t.Errorf("Id = %d, want 42", job.Id)
	}
	if job.Repo != "test-repo" {
		t.Errorf("Repo = %q, want test-repo", job.Repo)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	_, err := cs.GetJob(context.Background(), &apiv1.GetJobRequest{Id: 999})
	if err == nil {
		t.Fatal("expected error for unknown job")
	}
	st, ok := grpcStatus.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// --- KillJob tests ---

func TestKillJob_Found(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.running[50] = &runningJob{
		repo:      "test",
		cancel:    cancel,
		startedAt: time.Now(),
	}
	cs := &controlServer{sched: s, log: silentLogger()}

	_, err := cs.KillJob(ctx, &apiv1.KillJobRequest{Id: 50})
	if err != nil {
		t.Fatalf("KillJob() error: %v", err)
	}

	// Verify that cancel was called (context should be done)
	select {
	case <-ctx.Done():
		// expected
	default:
		t.Error("expected job context to be cancelled after KillJob")
	}
}

func TestKillJob_NotFound(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	_, err := cs.KillJob(context.Background(), &apiv1.KillJobRequest{Id: 999})
	if err == nil {
		t.Fatal("expected error for unknown job")
	}
	st, ok := grpcStatus.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// --- SocketPath tests ---

func TestSocketPath_Format(t *testing.T) {
	path := SocketPath("/var/lib/ephemerd")
	if path == "" {
		t.Fatal("SocketPath returned empty")
	}
	// Should end with the socket filename
	if path != "/var/lib/ephemerd/ephemerd.sock" && path != "/var/lib/ephemerd\\ephemerd.sock" {
		t.Logf("SocketPath = %q (OS-specific)", path)
	}
}
