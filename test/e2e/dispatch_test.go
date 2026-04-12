//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// TestE2E_Dispatch_RoundTrip starts a dispatch gRPC server backed by a real
// containerd Runtime, then creates a container through the dispatch client,
// waits for it to exit, and destroys it. This exercises the full gRPC dispatch
// path that the Windows host uses to dispatch Linux jobs to the WSL worker.
func TestE2E_Dispatch_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Set up networking
	netDataDir := filepath.Join(sharedDataDir, "net-dispatch")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI: %v", err)
	}

	net, err := networking.New(networking.Config{
		DataDir:   netDataDir,
		CNIBinDir: cm.Dir(),
		Log:       sharedLog,
	})
	if err != nil {
		t.Fatalf("networking: %v", err)
	}
	defer net.Cleanup()

	if err := net.InstallFirewallRules(); err != nil {
		sharedLog.Warn("firewall", "error", err)
	}

	logDir := filepath.Join(sharedDataDir, "logs-dispatch")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rt, err := runtime.New(runtime.Config{
		Client:  sharedCtrd.Client(),
		Network: net,
		LogDir:  logDir,
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	// Start dispatch server on a random high port
	port := 19000 + int(time.Now().UnixNano()%1000)
	cleanup := scheduler.StartDispatchServer(port, rt, sharedLog)
	defer cleanup()

	// Give the server a moment to start
	time.Sleep(500 * time.Millisecond)

	// Connect client
	addr := fmt.Sprintf("localhost:%d", port)
	client, err := scheduler.NewDispatchClient(addr)
	if err != nil {
		t.Fatalf("NewDispatchClient(%s): %v", addr, err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("client close: %v", err)
		}
	}()

	containerID := fmt.Sprintf("e2e-dispatch-%d", time.Now().UnixNano())

	// Create — uses default image with fake jitconfig, runner will fail immediately
	t.Logf("dispatch Create: %s", containerID)
	if err := client.Create(ctx, containerID, "", "fake-jit-config-dispatch"); err != nil {
		t.Fatalf("dispatch Create: %v", err)
	}

	// Wait — should return quickly since the runner fails on bad config
	t.Logf("dispatch Wait: %s", containerID)
	exitCode, err := client.Wait(ctx, containerID)
	if err != nil {
		t.Logf("dispatch Wait error (expected for fake config): %v", err)
	}
	t.Logf("dispatch Wait returned exit code %d", exitCode)

	// Destroy — clean up the container
	t.Logf("dispatch Destroy: %s", containerID)
	if err := client.Destroy(ctx, containerID); err != nil {
		t.Fatalf("dispatch Destroy: %v", err)
	}

	t.Log("dispatch round-trip passed")
}

// TestE2E_Dispatch_DestroyUnknownJob verifies that destroying a job that
// doesn't exist returns an appropriate error.
func TestE2E_Dispatch_DestroyUnknownJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	rt, err := runtime.New(runtime.Config{
		Client: sharedCtrd.Client(),
		Log:    sharedLog,
		LogDir: filepath.Join(sharedDataDir, "logs-dispatch-unknown"),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	port := 19100 + int(time.Now().UnixNano()%1000)
	cleanup := scheduler.StartDispatchServer(port, rt, sharedLog)
	defer cleanup()

	time.Sleep(500 * time.Millisecond)

	client, err := scheduler.NewDispatchClient(fmt.Sprintf("localhost:%d", port))
	if err != nil {
		t.Fatalf("NewDispatchClient: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("client close: %v", err)
		}
	}()

	err = client.Destroy(ctx, "nonexistent-job-id")
	if err == nil {
		t.Fatal("expected error for destroying unknown job")
	}
	t.Logf("destroy unknown job error (expected): %v", err)
}
