//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/dind"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// TestE2E_Dind_PingVersion verifies the fake Docker daemon is reachable
// from inside a container via /var/run/docker.sock.
func TestE2E_Dind_PingVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, e2eNamespace)

	// Start fake Docker daemon
	dindSrv, err := dind.New(dind.Config{
		JobID:   "e2e-dind-ping",
		DataDir: sharedDataDir,
		Client:  ctrdClient,
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := dindSrv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer dindSrv.Stop()

	// Pull an image with curl/wget available for testing
	testImage := "docker.io/library/alpine:latest"
	if _, err := ctrdClient.GetImage(nsCtx, testImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, testImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling image: %v", err)
		}
	}
	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	containerID := fmt.Sprintf("e2e-dind-ping-%d", time.Now().UnixNano())

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithProcessArgs("sleep", "86400"),
		oci.WithNewPrivileges,
		withDockerSocketMount(dindSrv.SocketPath()),
	}

	container, err := ctrdClient.NewContainer(nsCtx, containerID,
		client.WithImage(img),
		client.WithNewSnapshot(containerID+"-snapshot", img),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	defer func() { _ = container.Delete(nsCtx, client.WithSnapshotCleanup) }()

	task, err := container.NewTask(nsCtx, cio.NullIO)
	if err != nil {
		t.Fatalf("creating task: %v", err)
	}
	defer func() {
		if status, serr := task.Status(nsCtx); serr == nil && status.Status == client.Running {
			_ = task.Kill(nsCtx, 9)
			exitCh, werr := task.Wait(nsCtx)
			if werr == nil {
				<-exitCh
			}
		}
		_, _ = task.Delete(nsCtx, client.WithProcessKill)
	}()

	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	// Install curl inside the container and test the Docker socket
	steps := []struct {
		name string
		cmd  string
	}{
		{"Install curl", "apk add --no-cache curl"},
		{"Ping docker socket", `curl -s --unix-socket /var/run/docker.sock http://docker/_ping`},
		{"Check version", `curl -s --unix-socket /var/run/docker.sock http://docker/version | grep -q ephemerd`},
		{"Check info", `curl -s --unix-socket /var/run/docker.sock http://docker/info | grep -q ephemerd-dind`},
		{"List images (empty)", `curl -s --unix-socket /var/run/docker.sock http://docker/images/json | grep -q '\[\]'`},
		{"Versioned endpoint", `curl -s --unix-socket /var/run/docker.sock http://docker/v1.45/version | grep -q ephemerd`},
	}

	for i, step := range steps {
		t.Logf("--- Step: %s", step.name)
		exitCode := execInContainer(t, nsCtx, task, containerID, i, step.cmd)
		if exitCode != 0 {
			t.Fatalf("step %q failed with exit code %d", step.name, exitCode)
		}
		t.Logf("    passed")
	}

	t.Log("==> Docker socket ping/version test passed")
}

// TestE2E_Dind_DockerCLI verifies that the actual Docker CLI works against
// the fake daemon — docker info, docker pull, docker images.
func TestE2E_Dind_DockerCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	nsCtx := namespaces.WithNamespace(ctx, e2eNamespace)

	// Start fake Docker daemon
	dindSrv, err := dind.New(dind.Config{
		JobID:   "e2e-dind-cli",
		DataDir: sharedDataDir,
		Client:  ctrdClient,
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := dindSrv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer dindSrv.Stop()

	// Use an image that has Docker CLI pre-installed
	dockerImage := "docker.io/library/docker:cli"
	if _, err := ctrdClient.GetImage(nsCtx, dockerImage); err != nil {
		t.Log("pulling docker:cli image...")
		if _, err := ctrdClient.Pull(nsCtx, dockerImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling docker:cli: %v", err)
		}
	}
	img, err := ctrdClient.GetImage(nsCtx, dockerImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	containerID := fmt.Sprintf("e2e-dind-cli-%d", time.Now().UnixNano())
	netDataDir := filepath.Join(sharedDataDir, "net-dind-cli")
	os.MkdirAll(netDataDir, 0o755)

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithProcessArgs("sleep", "86400"),
		oci.WithNewPrivileges,
		withDockerSocketMount(dindSrv.SocketPath()),
		withDNSMount(netDataDir, containerID),
	}

	container, err := ctrdClient.NewContainer(nsCtx, containerID,
		client.WithImage(img),
		client.WithNewSnapshot(containerID+"-snapshot", img),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	defer func() { _ = container.Delete(nsCtx, client.WithSnapshotCleanup) }()

	task, err := container.NewTask(nsCtx, cio.NullIO)
	if err != nil {
		t.Fatalf("creating task: %v", err)
	}
	defer func() {
		if status, serr := task.Status(nsCtx); serr == nil && status.Status == client.Running {
			_ = task.Kill(nsCtx, 9)
			exitCh, werr := task.Wait(nsCtx)
			if werr == nil {
				<-exitCh
			}
		}
		_, _ = task.Delete(nsCtx, client.WithProcessKill)
	}()

	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	steps := []struct {
		name string
		cmd  string
	}{
		{"docker version", "docker version"},
		{"docker info", "docker info"},
		{"docker images (empty)", "docker images"},
		{"docker pull alpine", "docker pull alpine:latest"},
		{"docker images (has alpine)", "docker images | grep alpine"},
	}

	for i, step := range steps {
		t.Logf("--- Step: %s", step.name)
		exitCode := execInContainer(t, nsCtx, task, containerID, i, step.cmd)
		if exitCode != 0 {
			t.Fatalf("step %q failed with exit code %d", step.name, exitCode)
		}
		t.Logf("    passed")
	}

	t.Log("==> Docker CLI test passed")
}

// execInContainer runs a command inside a running task and returns the exit code.
func execInContainer(t *testing.T, ctx context.Context, task client.Task, containerID string, stepIdx int, cmd string) uint32 {
	t.Helper()

	execID := fmt.Sprintf("step-%d-%d", stepIdx, time.Now().UnixNano())
	pspec := &ocispec.Process{
		Args: []string{"/bin/sh", "-e", "-c", cmd},
		Cwd:  "/",
		User: ocispec.User{UID: 0, GID: 0},
	}

	process, err := task.Exec(ctx, execID, pspec, cio.NewCreator(cio.WithStdio))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	exitCh, err := process.Wait(ctx)
	if err != nil {
		process.Delete(ctx, client.WithProcessKill)
		t.Fatalf("wait: %v", err)
	}

	if err := process.Start(ctx); err != nil {
		process.Delete(ctx, client.WithProcessKill)
		t.Fatalf("start: %v", err)
	}

	select {
	case status := <-exitCh:
		process.Delete(ctx)
		return status.ExitCode()
	case <-ctx.Done():
		process.Delete(ctx, client.WithProcessKill)
		t.Fatalf("timed out")
		return 1
	}
}

// withDockerSocketMount binds the fake Docker socket into the container.
func withDockerSocketMount(hostSocketPath string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/var/run/docker.sock",
			Type:        "bind",
			Source:      hostSocketPath,
			Options:     []string{"rbind", "rw"},
		})
		return nil
	}
}

// isDockerInPath checks if the Docker CLI is available.
func isDockerInPath() bool {
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		if _, err := os.Stat(filepath.Join(dir, "docker")); err == nil {
			return true
		}
	}
	return false
}
