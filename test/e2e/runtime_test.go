//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// TestE2E_Runtime_PullImage verifies that Runtime.PullImage can pull a
// lightweight image and that subsequent pulls are no-ops.
func TestE2E_Runtime_PullImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rt, err := runtime.New(runtime.Config{
		Client: sharedCtrd.Client(),
		Log:    sharedLog,
		LogDir: filepath.Join(sharedDataDir, "logs"),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	testImage := "docker.io/library/alpine:latest"

	// First pull
	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("PullImage (first): %v", err)
	}

	// Second pull should be a no-op (image already cached)
	if err := rt.PullImage(ctx, testImage); err != nil {
		t.Fatalf("PullImage (cached): %v", err)
	}

	// Verify the image exists in containerd
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")
	_, err = sharedCtrd.Client().GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("image not found after pull: %v", err)
	}
}

// TestE2E_Runtime_CleanOrphans verifies that CleanOrphans removes containers
// left over from a previous run.
func TestE2E_Runtime_CleanOrphans(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")

	// Pull alpine for test containers
	testImage := "docker.io/library/alpine:latest"
	if _, err := ctrdClient.GetImage(nsCtx, testImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, testImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling alpine: %v", err)
		}
	}

	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	// Create an orphan container (simulate a crash — container exists but no daemon is managing it)
	orphanID := fmt.Sprintf("orphan-test-%d", time.Now().UnixNano())
	orphanSnap := orphanID + "-snapshot"

	orphan, err := ctrdClient.NewContainer(nsCtx, orphanID,
		client.WithImage(img),
		client.WithNewSnapshot(orphanSnap, img),
	)
	if err != nil {
		t.Fatalf("creating orphan container: %v", err)
	}

	// Verify the orphan exists
	_, err = ctrdClient.LoadContainer(nsCtx, orphanID)
	if err != nil {
		t.Fatalf("orphan should exist: %v", err)
	}

	// Run CleanOrphans
	rt, err := runtime.New(runtime.Config{
		Client: ctrdClient,
		Log:    sharedLog,
		LogDir: filepath.Join(sharedDataDir, "logs"),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	if err := rt.CleanOrphans(ctx); err != nil {
		t.Fatalf("CleanOrphans: %v", err)
	}

	// Verify the orphan was removed
	_, err = ctrdClient.LoadContainer(nsCtx, orphanID)
	if err == nil {
		// Clean up manually if CleanOrphans didn't remove it
		if delErr := orphan.Delete(nsCtx, client.WithSnapshotCleanup); delErr != nil {
			t.Logf("manual cleanup: %v", delErr)
		}
		t.Fatal("orphan container should have been removed by CleanOrphans")
	}
	_ = orphan // suppress unused
}

// TestE2E_Runtime_CreateWaitDestroy exercises the full Runtime lifecycle with
// a fake JIT config. The runner will fail immediately (invalid config) but the
// container creation, wait, and destroy paths are all exercised.
func TestE2E_Runtime_CreateWaitDestroy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Set up networking
	netDataDir := filepath.Join(sharedDataDir, "net-runtime")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}

	net, err := networking.New(networking.Config{
		DataDir:   netDataDir,
		CNIBinDir: cm.Dir(),
		Log:       sharedLog,
	})
	if err != nil {
		t.Fatalf("initializing networking: %v", err)
	}
	defer net.Cleanup()

	if err := net.InstallFirewallRules(); err != nil {
		sharedLog.Warn("failed to install firewall rules", "error", err)
	}

	logDir := filepath.Join(sharedDataDir, "logs-runtime")
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

	// Create with empty image (uses default) and fake jitconfig.
	// The container will start, the runner entrypoint will fail immediately
	// because "fake-jit-config" isn't valid base64, and the task will exit.
	containerID := fmt.Sprintf("e2e-rt-%d", time.Now().UnixNano())

	env, err := rt.Create(ctx, containerID, "", "fake-jit-config")
	if err != nil {
		t.Fatalf("Runtime.Create: %v", err)
	}

	// Wait for the task to exit (should be quick — runner fails on bad config)
	exitCode, err := rt.Wait(ctx, env)
	if err != nil {
		t.Logf("Runtime.Wait error (expected for fake config): %v", err)
	}
	t.Logf("container exited with code %d (expected non-zero for fake config)", exitCode)

	// Destroy should clean up everything
	if err := rt.Destroy(ctx, env); err != nil {
		t.Fatalf("Runtime.Destroy: %v", err)
	}

	// Verify container is gone
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")
	_, err = sharedCtrd.Client().LoadContainer(nsCtx, containerID)
	if err == nil {
		t.Error("container should be removed after Destroy")
	}
}
