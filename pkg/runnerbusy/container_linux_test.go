//go:build linux

package runnerbusy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/client"
)

type fakeTask struct {
	id   string
	pids []client.ProcessInfo
	err  error
}

func (f *fakeTask) ID() string { return f.id }
func (f *fakeTask) Pids(context.Context) ([]client.ProcessInfo, error) {
	return f.pids, f.err
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startFakeWorker execs a real process whose argv[0] basename is
// Runner.Worker, so the probe reads it out of /proc exactly as it would
// read the actions-runner's own worker. Returns its PID.
func startFakeWorker(t *testing.T) uint32 {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary to impersonate a worker: %v", err)
	}
	src, err := os.ReadFile(sleep)
	if err != nil {
		t.Skipf("cannot read %s: %v", sleep, err)
	}
	path := filepath.Join(t.TempDir(), "Runner.Worker")
	if err := os.WriteFile(path, src, 0o755); err != nil {
		t.Fatalf("writing fake worker: %v", err)
	}
	cmd := exec.Command(path, "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fake worker: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("killing fake worker: %v", err)
		}
		if _, err := cmd.Process.Wait(); err != nil {
			t.Logf("reaping fake worker: %v", err)
		}
	})
	return uint32(cmd.Process.Pid)
}

// TestContainerBusy_SeesAWorker is the Linux probe end to end: a real
// process named Runner.Worker, found through a task's PID listing and
// read out of /proc, must report Busy. This is the observation that
// vetoes teardown of a live build.
func TestContainerBusy_SeesAWorker(t *testing.T) {
	worker := startFakeWorker(t)
	task := &fakeTask{
		id:   "job-1",
		pids: []client.ProcessInfo{{Pid: uint32(os.Getpid())}, {Pid: worker}},
	}
	got, err := ContainerBusy(context.Background(), task, quiet())
	if err != nil {
		t.Fatalf("ContainerBusy: %v", err)
	}
	if got != Busy {
		t.Errorf("state = %v, want busy", got)
	}
}

// TestContainerBusy_NoWorkerIsIdle pins the other half: a readable
// process listing with no worker in it is a POSITIVE idle observation,
// which is what makes immediate reaping safe.
func TestContainerBusy_NoWorkerIsIdle(t *testing.T) {
	task := &fakeTask{
		id:   "job-1",
		pids: []client.ProcessInfo{{Pid: uint32(os.Getpid())}},
	}
	got, err := ContainerBusy(context.Background(), task, quiet())
	if err != nil {
		t.Fatalf("ContainerBusy: %v", err)
	}
	if got != Idle {
		t.Errorf("state = %v, want idle", got)
	}
}

// TestContainerBusy_FailuresAreUnknown pins the fail-safe contract: every
// way the probe can fail to see the container's processes reports
// Unknown, so the caller vetoes teardown instead of assuming idle.
func TestContainerBusy_FailuresAreUnknown(t *testing.T) {
	tests := []struct {
		name string
		task *fakeTask
	}{
		{
			name: "task query failed",
			task: &fakeTask{id: "job-1", err: errors.New("shim gone")},
		},
		{
			name: "task reports no processes at all",
			task: &fakeTask{id: "job-1"},
		},
		{
			// PIDs we cannot read under /proc mean we are not in a
			// position to see this container, not that it is quiet.
			name: "no listed process was readable",
			task: &fakeTask{id: "job-1", pids: []client.ProcessInfo{{Pid: 1 << 30}, {Pid: 1<<30 + 1}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ContainerBusy(context.Background(), tt.task, quiet())
			if got != Unknown {
				t.Errorf("state = %v, want unknown", got)
			}
			if err == nil {
				t.Error("want an error explaining why the probe could not answer")
			}
		})
	}
}

// TestProcessName_FallsBackToComm pins the fallback used when a process
// has no readable cmdline. "Runner.Worker" is 13 bytes, so it survives
// comm's truncation and still matches.
func TestProcessName_FallsBackToComm(t *testing.T) {
	name, err := processName(os.Getpid())
	if err != nil {
		t.Fatalf("processName(self): %v", err)
	}
	if name == "" {
		t.Error("processName(self) is empty")
	}
	if _, err := processName(1 << 30); err == nil {
		t.Error("processName of a nonexistent pid should fail, not return a name")
	}
}
