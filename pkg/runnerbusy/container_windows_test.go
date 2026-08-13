//go:build windows

package runnerbusy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Microsoft/hcsshim"
	"github.com/containerd/containerd/v2/client"
)

type fakeTask struct{ id string }

func (f *fakeTask) ID() string { return f.id }
func (f *fakeTask) Pids(context.Context) ([]client.ProcessInfo, error) {
	return nil, errors.New("the windows probe must not need containerd PIDs")
}

type fakeCompute struct {
	list     []hcsshim.ProcessListItem
	listErr  error
	closeErr error
	closed   bool
}

func (f *fakeCompute) ProcessList() ([]hcsshim.ProcessListItem, error) {
	return f.list, f.listErr
}

func (f *fakeCompute) Close() error {
	f.closed = true
	return f.closeErr
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func withCompute(t *testing.T, c hcsProcessLister, err error) {
	t.Helper()
	prev := openCompute
	openCompute = func(string) (hcsProcessLister, error) {
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	t.Cleanup(func() { openCompute = prev })
}

// TestContainerBusy_Windows pins the HCS probe. Hyper-V isolated
// containers keep their processes in a utility VM, invisible to the host
// process table, so the compute system's own process list — reported
// through the GCS with an ImageName per process — is the only ground
// truth available.
func TestContainerBusy_Windows(t *testing.T) {
	tests := []struct {
		name     string
		list     []hcsshim.ProcessListItem
		listErr  error
		openErr  error
		want     State
		wantErr  bool
		wantOpen bool
	}{
		{
			name: "worker present means busy",
			list: []hcsshim.ProcessListItem{
				{ProcessId: 1, ImageName: "Runner.Listener.exe"},
				{ProcessId: 2, ImageName: "Runner.Worker.exe"},
			},
			want:     Busy,
			wantOpen: true,
		},
		{
			// The listener alone is what an IDLE runner looks like: it is
			// alive for the runner's whole life. Treating "a runner
			// process exists" as busy would veto every teardown forever.
			name: "listener alone means idle",
			list: []hcsshim.ProcessListItem{
				{ProcessId: 1, ImageName: "Runner.Listener.exe"},
				{ProcessId: 3, ImageName: "cmd.exe"},
			},
			want:     Idle,
			wantOpen: true,
		},
		{
			name:     "empty process list is unknown, not idle",
			want:     Unknown,
			wantErr:  true,
			wantOpen: true,
		},
		{
			name:     "process list error is unknown",
			listErr:  errors.New("compute system is shutting down"),
			want:     Unknown,
			wantErr:  true,
			wantOpen: true,
		},
		{
			name:    "cannot open the compute system",
			openErr: errors.New("no such compute system"),
			want:    Unknown,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCompute{list: tt.list, listErr: tt.listErr}
			withCompute(t, fake, tt.openErr)

			got, err := ContainerBusy(context.Background(), &fakeTask{id: "job-1"}, quiet())
			if got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if fake.closed != tt.wantOpen {
				t.Errorf("compute system closed = %v, want %v", fake.closed, tt.wantOpen)
			}
		})
	}
}

// TestContainerBusy_WindowsCloseErrorIsNotFatal pins that a failure to
// release the HCS handle does not change the verdict — the answer is
// already computed and the handle is process-local.
func TestContainerBusy_WindowsCloseErrorIsNotFatal(t *testing.T) {
	fake := &fakeCompute{
		list:     []hcsshim.ProcessListItem{{ProcessId: 2, ImageName: "Runner.Worker.exe"}},
		closeErr: errors.New("handle already gone"),
	}
	withCompute(t, fake, nil)

	got, err := ContainerBusy(context.Background(), &fakeTask{id: "job-1"}, quiet())
	if err != nil {
		t.Fatalf("ContainerBusy: %v", err)
	}
	if got != Busy {
		t.Errorf("state = %v, want busy", got)
	}
}
