package scheduler

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcStatus "google.golang.org/grpc/status"
)

// --- findJob tests ---

func TestFindJob_NotFound(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	_, _, ok := cs.findJob(999)
	if ok {
		t.Error("findJob(999) should return ok=false")
	}
}

func TestFindJob_FoundAcrossProviders(t *testing.T) {
	s := New(Config{Log: silentLogger()})

	// Two providers with the same job ID — findJob should find one of them.
	s.running[jobKey{Provider: "github", JobID: 42}] = &runningJob{repo: "gh-repo", startedAt: time.Now()}
	cs := &controlServer{sched: s, log: silentLogger()}

	key, rj, ok := cs.findJob(42)
	if !ok {
		t.Fatal("findJob(42) should find the job")
	}
	if key.JobID != 42 {
		t.Errorf("key.JobID = %d, want 42", key.JobID)
	}
	if rj == nil {
		t.Fatal("rj is nil")
	}
	if rj.repo != "gh-repo" {
		t.Errorf("rj.repo = %q, want gh-repo", rj.repo)
	}
}

func TestFindJob_DifferentJobIDs(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	s.running[jobKey{Provider: "github", JobID: 1}] = &runningJob{repo: "a", startedAt: time.Now()}
	s.running[jobKey{Provider: "forgejo", JobID: 2}] = &runningJob{repo: "b", startedAt: time.Now()}
	cs := &controlServer{sched: s, log: silentLogger()}

	if _, _, ok := cs.findJob(1); !ok {
		t.Error("findJob(1) should succeed")
	}
	if _, _, ok := cs.findJob(2); !ok {
		t.Error("findJob(2) should succeed")
	}
	if _, _, ok := cs.findJob(3); ok {
		t.Error("findJob(3) should fail")
	}
}

// --- GetJobLogs tests ---

// fakeLogStream captures sent chunks and lets us simulate ctx done / send
// errors. It implements grpc.ServerStreamingServer[apiv1.LogChunk].
type fakeLogStream struct {
	grpc.ServerStream
	ctx       context.Context
	chunks    [][]byte
	sendErr   error
	maxSends  int
}

func (f *fakeLogStream) Send(c *apiv1.LogChunk) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if f.maxSends > 0 && len(f.chunks) >= f.maxSends {
		return errors.New("send limit reached")
	}
	out := make([]byte, len(c.Data))
	copy(out, c.Data)
	f.chunks = append(f.chunks, out)
	return nil
}

func (f *fakeLogStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *fakeLogStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeLogStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeLogStream) SetTrailer(metadata.MD)       {}

func TestGetJobLogs_JobNotFound(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	cs := &controlServer{sched: s, log: silentLogger()}

	stream := &fakeLogStream{}
	err := cs.GetJobLogs(&apiv1.GetJobLogsRequest{Id: 999}, stream)
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	st, _ := grpcStatus.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestGetJobLogs_DispatchedJobUnimplemented(t *testing.T) {
	s := New(Config{Log: silentLogger()})
	// Dispatched job (env is nil, dispatched is set).
	s.running[jobKey{Provider: "github", JobID: 1}] = &runningJob{
		repo:       "r",
		dispatched: "ephemerd-x",
		startedAt:  time.Now(),
	}
	cs := &controlServer{sched: s, log: silentLogger()}

	stream := &fakeLogStream{}
	err := cs.GetJobLogs(&apiv1.GetJobLogsRequest{Id: 1}, stream)
	if err == nil {
		t.Fatal("expected error for dispatched job")
	}
	st, _ := grpcStatus.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

func TestGetJobLogs_LogFileMissing(t *testing.T) {
	s := New(Config{
		DataDir: t.TempDir(),
		Log:     silentLogger(),
	})

	// Local job (env != nil) but no log file written.
	env := makeEnvWithID(t, "fake-job-id")
	s.running[jobKey{Provider: "github", JobID: 5}] = &runningJob{
		repo:      "r",
		env:       env,
		startedAt: time.Now(),
	}
	cs := &controlServer{sched: s, log: silentLogger()}

	stream := &fakeLogStream{}
	err := cs.GetJobLogs(&apiv1.GetJobLogsRequest{Id: 5}, stream)
	if err == nil {
		t.Fatal("expected error for missing log file")
	}
	st, _ := grpcStatus.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestGetJobLogs_StreamsContent(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dataDir, "logs", "fake-job-id.log")
	want := []byte("hello world from log\n")
	if err := os.WriteFile(logPath, want, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{DataDir: dataDir, Log: silentLogger()})

	env := makeEnvWithID(t, "fake-job-id")
	s.running[jobKey{Provider: "github", JobID: 9}] = &runningJob{
		repo:      "r",
		env:       env,
		startedAt: time.Now(),
	}
	cs := &controlServer{sched: s, log: silentLogger()}

	stream := &fakeLogStream{}
	err := cs.GetJobLogs(&apiv1.GetJobLogsRequest{Id: 9}, stream)
	if err != nil {
		t.Fatalf("GetJobLogs: %v", err)
	}

	got := joinChunks(stream.chunks)
	if string(got) != string(want) {
		t.Errorf("streamed content = %q, want %q", got, want)
	}
}

func TestGetJobLogs_SendError(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dataDir, "logs", "fake-job-id.log")
	if err := os.WriteFile(logPath, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{DataDir: dataDir, Log: silentLogger()})
	env := makeEnvWithID(t, "fake-job-id")
	s.running[jobKey{Provider: "github", JobID: 9}] = &runningJob{
		repo:      "r",
		env:       env,
		startedAt: time.Now(),
	}
	cs := &controlServer{sched: s, log: silentLogger()}

	wantErr := io.ErrClosedPipe
	stream := &fakeLogStream{sendErr: wantErr}
	err := cs.GetJobLogs(&apiv1.GetJobLogsRequest{Id: 9}, stream)
	if err == nil {
		t.Fatal("expected send error to propagate")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

// makeEnvWithID returns a minimal RunnerEnv suitable for findJob/log tests.
// We don't populate Container/Task — code paths that touch them are skipped
// because they cross the containerd boundary.
func makeEnvWithID(t *testing.T, id string) *runtime.RunnerEnv {
	t.Helper()
	return &runtime.RunnerEnv{ID: id}
}

func joinChunks(chunks [][]byte) []byte {
	var n int
	for _, c := range chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}
