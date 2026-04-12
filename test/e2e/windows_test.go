//go:build e2e && windows

package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// testLogWindows returns a logger for Windows e2e tests.
func testLogWindows() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestE2E_Windows_WSLDispatch starts a WSL Linux VM and verifies the dispatch
// gRPC round-trip works: the Windows host creates a Linux container inside
// WSL via the dispatch server and waits for it to complete.
//
// Prerequisites:
// - WSL2 enabled
// - The ephemerd Linux binary embedded (mage build:windows)
// - The pre-built rootfs embedded
//
// Run with:
//
//	go test -tags e2e -run TestE2E_Windows_WSLDispatch -v -timeout 10m ./test/e2e/
func TestE2E_Windows_WSLDispatch(t *testing.T) {
	log := testLogWindows()
	dataDir := filepath.Join(os.TempDir(), "ephemerd-e2e-wsl")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cfg := vm.LinuxVMConfig{
		DataDir:        dataDir,
		CPUs:           2,
		MemoryMB:       2048,
		ContainerdPort: 11000,
		Log:            log,
	}

	// Start the Linux VM (WSL2 distro)
	t.Log("starting Linux VM via WSL2")
	linuxVM, err := vm.StartLinuxVM(cfg)
	if err != nil {
		t.Fatalf("StartLinuxVM: %v", err)
	}
	defer linuxVM.Stop()

	// Verify containerd client is available
	ctrdClient := linuxVM.Client()
	if ctrdClient == nil {
		t.Fatal("LinuxVM.Client() returned nil")
	}

	ver, err := ctrdClient.Version(ctx)
	if err != nil {
		t.Fatalf("containerd version: %v", err)
	}
	t.Logf("WSL containerd version: %s", ver.Version)

	// Verify dispatch address is available
	dispatchAddr := linuxVM.DispatchAddr()
	if dispatchAddr == "" {
		t.Fatal("LinuxVM.DispatchAddr() returned empty")
	}
	t.Logf("dispatch address: %s", dispatchAddr)

	// Connect dispatch client and do a round-trip
	dispatchClient, err := scheduler.NewDispatchClient(dispatchAddr)
	if err != nil {
		t.Fatalf("NewDispatchClient(%s): %v", dispatchAddr, err)
	}
	defer func() {
		if err := dispatchClient.Close(); err != nil {
			t.Logf("dispatch client close: %v", err)
		}
	}()

	containerID := fmt.Sprintf("e2e-wsl-%d", time.Now().UnixNano())

	// Create a Linux container in WSL with a fake JIT config
	t.Logf("creating container %s via dispatch", containerID)
	if err := dispatchClient.Create(ctx, containerID, "", "fake-wsl-jit"); err != nil {
		t.Fatalf("dispatch Create: %v", err)
	}

	// Wait for it to exit (runner will fail on bad config)
	exitCode, err := dispatchClient.Wait(ctx, containerID)
	if err != nil {
		t.Logf("dispatch Wait error (expected for fake config): %v", err)
	}
	t.Logf("dispatch Wait exit code: %d", exitCode)

	// Destroy
	if err := dispatchClient.Destroy(ctx, containerID); err != nil {
		t.Fatalf("dispatch Destroy: %v", err)
	}

	t.Log("WSL dispatch round-trip passed")
}

// TestE2E_Windows_WSLDistro_BootShutdown verifies the WSL distro lifecycle
// without running any containers — just boot the VM and tear it down cleanly.
func TestE2E_Windows_WSLDistro_BootShutdown(t *testing.T) {
	log := testLogWindows()
	dataDir := filepath.Join(os.TempDir(), "ephemerd-e2e-wsl-boot")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}()

	cfg := vm.LinuxVMConfig{
		DataDir:        dataDir,
		CPUs:           2,
		MemoryMB:       2048,
		ContainerdPort: 11100,
		Log:            log,
	}

	t.Log("starting Linux VM")
	linuxVM, err := vm.StartLinuxVM(cfg)
	if err != nil {
		t.Fatalf("StartLinuxVM: %v", err)
	}

	// Verify we can reach containerd
	ver, err := linuxVM.Client().Version(context.Background())
	if err != nil {
		linuxVM.Stop()
		t.Fatalf("containerd not reachable: %v", err)
	}
	t.Logf("containerd ready: %s", ver.Version)

	// Clean shutdown
	t.Log("stopping Linux VM")
	linuxVM.Stop()

	t.Log("WSL boot/shutdown test passed")
}
