package scheduler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// blockingMacVM is a vm.MacOSVM whose WaitForRunner never returns on its own —
// it only unblocks when Stop() is called (or its ctx is cancelled). This models
// the incident: a hung guest SSH command inside WaitForRunner that the VM's own
// polling loop cannot interrupt, so the only way out is to kill the VM.
type blockingMacVM struct {
	stopped   chan struct{}
	stopCount atomic.Int32
	stopOnce  sync.Once
}

func (m *blockingMacVM) WriteJITConfig(string) error   { return nil }
func (m *blockingMacVM) Start(_ context.Context) error { return nil }
func (m *blockingMacVM) RunnerAddress() string         { return "" }
func (m *blockingMacVM) Wait(context.Context) (int, error) {
	return 0, nil
}

func (m *blockingMacVM) WaitForRunner(ctx context.Context) (string, error) {
	select {
	case <-m.stopped:
		return "", fmt.Errorf("macOS VM exited before runner became reachable")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *blockingMacVM) Stop() {
	m.stopCount.Add(1)
	m.stopOnce.Do(func() { close(m.stopped) })
}

// TestHandleMacOSJob_ProvisionTimeoutFreesSlot reproduces the 2026-08-21 mac
// runner incident in miniature: a macOS VM wedges in the "wait for runner to
// become reachable" phase and never registers. The provisioning watchdog must
// force-stop the VM on the deadline (which unblocks the wait), release the sole
// macOS concurrency slot, and deregister the ghost runner — so macOS CI self-
// heals instead of stalling fleet-wide until a human kills the process.
func TestHandleMacOSJob_ProvisionTimeoutFreesSlot(t *testing.T) {
	prov := newMockProvider("mac-prov")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	vmFake := &blockingMacVM{stopped: make(chan struct{})}

	s := New(Config{
		Providers:             []providers.Provider{prov},
		MacOSVMConfig:         &vm.MacOSVMConfig{},
		MaxMacOSVMs:           1, // the incident's max_concurrent = 1
		MacOSProvisionTimeout: 100 * time.Millisecond,
		Log:                   quietLogger(),
	})
	s.newMacOSVM = func(_ vm.MacOSVMConfig, _ string) (vm.MacOSVM, error) {
		return vmFake, nil
	}

	event := providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "repo",
		JobID:    96942856235,
		Labels:   []string{"self-hosted", "macos"},
	}

	done := make(chan struct{})
	go func() {
		s.handleMacOSJob(context.Background(), event)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleMacOSJob did not return: the reachability wait was not bounded")
	}

	// The VM must have been force-stopped when the deadline fired.
	if vmFake.stopCount.Load() == 0 {
		t.Error("expected the macOS VM to be force-stopped on provisioning timeout")
	}

	// The sole macOS slot must be free again — a subsequent job can acquire it.
	select {
	case s.macSem <- struct{}{}:
		<-s.macSem
	default:
		t.Error("macOS concurrency slot was not released after the provisioning timeout")
	}

	// The claimed-but-never-run JIT runner must be deregistered.
	prov.mu.Lock()
	releases := len(prov.releases)
	prov.mu.Unlock()
	if releases == 0 {
		t.Error("expected ReleaseJob to deregister the ghost runner on timeout")
	}

	// Nothing should be left tracked as running.
	s.mu.Lock()
	running := len(s.running)
	s.mu.Unlock()
	if running != 0 {
		t.Errorf("running map has %d entries after timeout, want 0", running)
	}
}

// TestWaitForMacRunnerBounded_ReturnsWhenWaitSucceeds verifies the watchdog is
// transparent on the happy path: a VM that becomes reachable before the
// deadline is neither stopped nor reported as timed out.
func TestWaitForMacRunnerBounded_ReturnsWhenWaitSucceeds(t *testing.T) {
	s := New(Config{
		MacOSProvisionTimeout: 2 * time.Second,
		Log:                   quietLogger(),
	})

	var stops atomic.Int32
	quick := &fastMacVM{ip: "192.168.64.5", stops: &stops}

	ip, err := s.waitForMacRunnerBounded(context.Background(), quick, quietLogger())
	if err != nil {
		t.Fatalf("unexpected error on healthy provision: %v", err)
	}
	if ip != "192.168.64.5" {
		t.Errorf("ip = %q, want 192.168.64.5", ip)
	}
	if stops.Load() != 0 {
		t.Errorf("VM was stopped %d times on the happy path, want 0", stops.Load())
	}
}

// fastMacVM returns from WaitForRunner immediately with a fixed IP.
type fastMacVM struct {
	ip    string
	stops *atomic.Int32
}

func (m *fastMacVM) WriteJITConfig(string) error { return nil }
func (m *fastMacVM) Start(context.Context) error { return nil }
func (m *fastMacVM) RunnerAddress() string       { return m.ip }
func (m *fastMacVM) Wait(context.Context) (int, error) {
	return 0, nil
}
func (m *fastMacVM) WaitForRunner(context.Context) (string, error) {
	return m.ip, nil
}
func (m *fastMacVM) Stop() { m.stops.Add(1) }
