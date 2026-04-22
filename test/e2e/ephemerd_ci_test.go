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
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/networking"
)

// TestE2E_EphemerdCI runs ephemerd's own CI pipeline (`mage ci`) inside the
// containerized runner. This exercises the whole runtime stack end-to-end:
// containerd starts, the runner image is pulled, the repo is bind-mounted,
// networking works (mage downloads dependencies over the network), and the
// job completes with exit 0.
//
// This is the meta test: ephemerd running its own CI inside itself. If this
// passes, the runtime actually works for the canonical use case.
//
// Runtime is ~5-10 minutes (dominated by `mage download:all` fetching CNI
// plugins, runc, containerd-shim, and the Alpine rootfs build).
func TestE2E_EphemerdCI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	repoDir := findRepoRoot(t)

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}

	netDataDir := filepath.Join(sharedDataDir, "net-ci")
	if err := os.MkdirAll(netDataDir, 0o755); err != nil {
		t.Fatalf("creating net data dir: %v", err)
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

	// Pull the runner image — same one ephemerd uses for real jobs.
	nsCtx := namespaces.WithNamespace(ctx, e2eNamespace)
	runnerImage := "ghcr.io/actions/actions-runner:latest"

	sharedLog.Info("pulling runner image", "ref", runnerImage)
	if _, err := ctrdClient.GetImage(nsCtx, runnerImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, runnerImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling image: %v", err)
		}
	}

	img, err := ctrdClient.GetImage(nsCtx, runnerImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	gi := sniffGitInfo(repoDir)

	// Use a module cache inside the data dir so Go can download deps without
	// running as root on the host's $HOME.
	gopathMount := filepath.Join(sharedDataDir, "gopath")
	if err := os.MkdirAll(gopathMount, 0o755); err != nil {
		t.Fatalf("creating gopath dir: %v", err)
	}

	envVars := []string{
		"RUNNER_ALLOW_RUNASROOT=1",
		"GITHUB_WORKSPACE=/home/runner/_work/repo/repo",
		fmt.Sprintf("GITHUB_SHA=%s", gi.SHA),
		fmt.Sprintf("GITHUB_REF=%s", gi.Ref),
		fmt.Sprintf("GITHUB_REPOSITORY=%s", gi.Repository),
		"GITHUB_ACTIONS=true",
		"CI=true",
		"HOME=/root",
		"GOPATH=/go",
		"PATH=/usr/local/go/bin:/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}

	containerID := fmt.Sprintf("e2e-ci-%d", time.Now().UnixNano())

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv(envVars),
		oci.WithProcessArgs("sleep", "86400"),
		oci.WithNewPrivileges,
		withBindMount(repoDir, "/home/runner/_work/repo/repo"),
		withBindMount(gopathMount, "/go"),
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
	defer func() {
		if err := container.Delete(nsCtx, client.WithSnapshotCleanup); err != nil {
			t.Logf("failed to delete container: %v", err)
		}
	}()

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

	pid := task.Pid()
	netns := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := net.Setup(ctx, containerID, netns); err != nil {
		t.Fatalf("setting up network: %v", err)
	}
	defer func() { _ = net.Teardown(ctx, containerID, netns) }()

	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	// One-shot script: verify the toolchain, install mage, run `mage ci`.
	// `mage ci` is the same pipeline that gates every commit — golangci-lint,
	// `go test`, and a build. Success here means ephemerd can run its own CI.
	script := `
set -euo pipefail

echo "==> Go version"
go version

echo "==> Installing mage"
go install github.com/magefile/mage@latest

echo "==> Running mage ci"
cd /home/runner/_work/repo/repo
mage ci
`

	execID := fmt.Sprintf("ci-exec-%d", time.Now().UnixNano())
	pspec := &ocispec.Process{
		Args: []string{"/bin/bash", "-c", script},
		Env:  envVars,
		Cwd:  "/home/runner/_work/repo/repo",
		User: ocispec.User{UID: 0, GID: 0},
	}

	process, err := task.Exec(nsCtx, execID, pspec, cio.NewCreator(cio.WithStdio))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer func() {
		_, _ = process.Delete(nsCtx, client.WithProcessKill)
	}()

	exitCh, err := process.Wait(nsCtx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if err := process.Start(nsCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case status := <-exitCh:
		if status.ExitCode() != 0 {
			t.Fatalf("mage ci failed with exit code %d", status.ExitCode())
		}
		t.Logf("==> mage ci passed inside ephemerd container")
	case <-ctx.Done():
		t.Fatalf("mage ci timed out after %s", 20*time.Minute)
	}
}
