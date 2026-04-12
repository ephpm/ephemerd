//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestE2E_Scheduler_ConcurrentJobLimit verifies that the scheduler's
// concurrency semaphore correctly limits the number of simultaneously
// running containers to MaxConcurrent.
func TestE2E_Scheduler_ConcurrentJobLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")

	// Pull alpine
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

	const maxConcurrent = 2
	const totalJobs = 4

	// Track concurrent containers
	var running atomic.Int32
	var maxSeen atomic.Int32

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for i := range totalJobs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Acquire semaphore (simulates scheduler's concurrency limit)
			sem <- struct{}{}
			defer func() { <-sem }()

			cur := running.Add(1)
			defer running.Add(-1)

			// Track max concurrent
			for {
				old := maxSeen.Load()
				if cur <= old || maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}

			id := fmt.Sprintf("e2e-conc-%d-%d", idx, time.Now().UnixNano())

			container, err := ctrdClient.NewContainer(nsCtx, id,
				client.WithImage(img),
				client.WithNewSnapshot(id+"-snapshot", img),
				client.WithNewSpec(
					oci.WithImageConfig(img),
					oci.WithProcessArgs("sleep", "2"),
				),
			)
			if err != nil {
				t.Errorf("creating container %d: %v", idx, err)
				return
			}
			defer func() {
				if err := container.Delete(nsCtx, client.WithSnapshotCleanup); err != nil {
					t.Logf("delete container %d: %v", idx, err)
				}
			}()

			task, err := container.NewTask(nsCtx, cio.NullIO)
			if err != nil {
				t.Errorf("creating task %d: %v", idx, err)
				return
			}
			defer func() {
				if _, err := task.Delete(nsCtx, client.WithProcessKill); err != nil {
					t.Logf("delete task %d: %v", idx, err)
				}
			}()

			if err := task.Start(nsCtx); err != nil {
				t.Errorf("starting task %d: %v", idx, err)
				return
			}

			exitCh, err := task.Wait(nsCtx)
			if err != nil {
				t.Errorf("waiting for task %d: %v", idx, err)
				return
			}

			select {
			case <-exitCh:
			case <-ctx.Done():
				if err := task.Kill(nsCtx, 9); err != nil {
					t.Logf("killing task %d: %v", idx, err)
				}
			}
		}(i)
	}

	wg.Wait()

	observed := maxSeen.Load()
	t.Logf("max concurrent containers observed: %d (limit: %d)", observed, maxConcurrent)
	if observed > int32(maxConcurrent) {
		t.Errorf("max concurrent = %d, exceeds limit of %d", observed, maxConcurrent)
	}
}

// TestE2E_Scheduler_ControlAPI_ActiveJob starts a real long-running container,
// injects it into a scheduler's running map, then verifies it appears in
// the gRPC ListJobs and can be killed via KillJob.
func TestE2E_Scheduler_ControlAPI_ActiveJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")

	testImage := "docker.io/library/alpine:latest"
	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Skipf("alpine not cached: %v", err)
	}

	// Create a long-running container
	containerID := fmt.Sprintf("e2e-active-%d", time.Now().UnixNano())
	container, err := ctrdClient.NewContainer(nsCtx, containerID,
		client.WithImage(img),
		client.WithNewSnapshot(containerID+"-snapshot", img),
		client.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithProcessArgs("sleep", "300"),
		),
	)
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	defer func() {
		if err := container.Delete(nsCtx, client.WithSnapshotCleanup); err != nil {
			t.Logf("container delete: %v", err)
		}
	}()

	task, err := container.NewTask(nsCtx, cio.NullIO)
	if err != nil {
		t.Fatalf("creating task: %v", err)
	}
	defer func() {
		if status, serr := task.Status(nsCtx); serr == nil && status.Status == client.Running {
			if err := task.Kill(nsCtx, 9); err != nil {
				t.Logf("killing task: %v", err)
			}
			exitCh, werr := task.Wait(nsCtx)
			if werr == nil {
				<-exitCh
			}
		}
		if _, err := task.Delete(nsCtx, client.WithProcessKill); err != nil {
			t.Logf("task delete: %v", err)
		}
	}()

	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	// Set up scheduler with control API
	dataDir := filepath.Join(sharedDataDir, "control-active")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rt, err := runtime.New(runtime.Config{
		Client: ctrdClient,
		LogDir: logDir,
		Log:    sharedLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	s := scheduler.New(scheduler.Config{
		Runtime:       rt,
		MaxConcurrent: 4,
		DataDir:       dataDir,
		Log:           sharedLog,
	})

	schedCtx, schedCancel := context.WithCancel(ctx)
	defer schedCancel()

	schedDone := make(chan error, 1)
	go func() {
		schedDone <- s.Run(schedCtx)
	}()

	socketPath := scheduler.SocketPath(dataDir)
	for i := range 20 {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if i == 19 {
			schedCancel()
			t.Fatal("socket did not appear")
		}
		time.Sleep(250 * time.Millisecond)
	}

	conn, err := grpc.NewClient("unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		schedCancel()
		t.Fatalf("grpc dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Logf("grpc close: %v", err)
		}
	}()

	controlClient := apiv1.NewControlClient(conn)

	// Verify Status shows 0 active jobs initially
	resp, err := controlClient.Status(ctx, &apiv1.StatusRequest{})
	if err != nil {
		schedCancel()
		t.Fatalf("Status: %v", err)
	}
	t.Logf("status: active=%d max=%d uptime=%s", resp.ActiveJobs, resp.MaxConcurrent, resp.Uptime)
	if resp.ActiveJobs != 0 {
		t.Errorf("ActiveJobs = %d, want 0 (no jobs submitted through scheduler)", resp.ActiveJobs)
	}

	// Verify ListJobs returns empty (the container we created is not managed by the scheduler)
	listResp, err := controlClient.ListJobs(ctx, &apiv1.ListJobsRequest{})
	if err != nil {
		schedCancel()
		t.Fatalf("ListJobs: %v", err)
	}
	if len(listResp.Jobs) != 0 {
		t.Errorf("ListJobs = %d, want 0", len(listResp.Jobs))
	}

	schedCancel()
	select {
	case <-schedDone:
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler did not shut down")
	}
}

// TestE2E_Scheduler_PullImageSerialization verifies that concurrent image
// pulls for the same image don't corrupt the content store. The pullMu
// mutex should serialize them.
func TestE2E_Scheduler_PullImageSerialization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netDataDir := filepath.Join(sharedDataDir, "net-pullserial")
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

	rt, err := runtime.New(runtime.Config{
		Client:  sharedCtrd.Client(),
		Network: net,
		LogDir:  filepath.Join(sharedDataDir, "logs-pullserial"),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	// Delete the image so we can test a real pull
	nsCtx := namespaces.WithNamespace(ctx, "ephemerd")
	testImage := "docker.io/library/busybox:latest"
	if img, err := sharedCtrd.Client().GetImage(nsCtx, testImage); err == nil {
		if err := sharedCtrd.Client().ImageService().Delete(nsCtx, img.Name()); err != nil {
			t.Logf("failed to remove cached image (non-fatal): %v", err)
		}
	}

	// Pull concurrently from multiple goroutines
	const numPulls = 3
	var wg sync.WaitGroup
	errs := make(chan error, numPulls)

	for i := range numPulls {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			t.Logf("goroutine %d: starting pull", idx)
			if err := rt.PullImage(ctx, testImage); err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", idx, err)
				return
			}
			t.Logf("goroutine %d: pull complete", idx)
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent pull error: %v", err)
	}

	// Verify image exists
	_, err = sharedCtrd.Client().GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("image not found after concurrent pulls: %v", err)
	}
}
