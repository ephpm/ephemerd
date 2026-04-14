//go:build e2e && privileged

package e2e

import (
	"context"
	"fmt"
	"log/slog"
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
	"os/exec"

	"github.com/ephpm/ephemerd/pkg/cni"
	ctdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/networking"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const e2eNamespace = "ephemerd"

// Shared infrastructure — containerd can only be started once per process
// due to global Prometheus metrics registration.
var (
	sharedCtrd    *ctdpkg.Server
	sharedDataDir string
	sharedLog     *slog.Logger
)

func TestMain(m *testing.M) {
	// When invoked as a subprocess by TestCrictl_*, re-enter as crictl
	// against the socket passed via env. The child exits via crictl.Main()
	// so m.Run() is never reached on this path.
	if sock := os.Getenv(crictlSocketEnv); sock != "" {
		args := strings.Split(os.Getenv(crictlArgsEnv), "\x00")
		_ = ctdpkg.ExecCrictl(sock, args)
		return
	}

	if os.Getuid() != 0 {
		fmt.Println("SKIP: e2e tests require root")
		os.Exit(0)
	}

	sharedLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Use a fixed data dir so we don't collide with a running ephemerd
	sharedDataDir = "/tmp/ephemerd-e2e"
	os.RemoveAll(sharedDataDir)
	os.MkdirAll(sharedDataDir, 0o755)

	// Start containerd once for all tests
	var err error
	sharedCtrd, err = ctdpkg.New(ctdpkg.Config{
		DataDir: sharedDataDir,
		Log:     sharedLog,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: starting containerd: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	sharedCtrd.Stop()
	os.RemoveAll(sharedDataDir)
	os.Exit(code)
}

func TestE2E_RunWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()
	repoDir := findRepoRoot(t)

	cm := cni.New(sharedDataDir, sharedLog)
	if err := cm.Extract(); err != nil {
		t.Fatalf("extracting CNI plugins: %v", err)
	}

	netDataDir := filepath.Join(sharedDataDir, "net-run")
	os.MkdirAll(netDataDir, 0o755)

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

	// Pull the runner image
	nsCtx := namespaces.WithNamespace(ctx, e2eNamespace)
	sharedLog.Info("pulling runner image", "ref", "ghcr.io/actions/actions-runner:latest")
	if _, err := ctrdClient.GetImage(nsCtx, "ghcr.io/actions/actions-runner:latest"); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, "ghcr.io/actions/actions-runner:latest", client.WithPullUnpack); err != nil {
			t.Fatalf("pulling image: %v", err)
		}
	}

	img, err := ctrdClient.GetImage(nsCtx, "ghcr.io/actions/actions-runner:latest")
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	// Sniff git info
	gi := sniffGitInfo(repoDir)

	// Build env vars matching what the workflow runner sets
	envVars := []string{
		"RUNNER_ALLOW_RUNASROOT=1",
		"GITHUB_WORKSPACE=/home/runner/_work/repo/repo",
		fmt.Sprintf("GITHUB_SHA=%s", gi.SHA),
		fmt.Sprintf("GITHUB_REF=%s", gi.Ref),
		fmt.Sprintf("GITHUB_REPOSITORY=%s", gi.Repository),
		"GITHUB_ACTIONS=true",
		"CI=true",
	}

	containerID := fmt.Sprintf("e2e-run-%d", time.Now().UnixNano())

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv(envVars),
		oci.WithProcessArgs("sleep", "86400"),
		oci.WithNewPrivileges,
		withBindMount(repoDir, "/home/runner/_work/repo/repo"),
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

	// Execute test steps via task.Exec
	steps := []struct {
		name string
		cmd  string
	}{
		{"Hello", `echo "hello from ephemerd e2e"`},
		{"Check repo mount", "test -f /home/runner/_work/repo/repo/go.mod && echo 'repo mounted OK'"},
		{"System info", "uname -a"},
		{"Check DNS config", "cat /etc/resolv.conf"},
		{"Network test", `nslookup github.com 1.1.1.1 || echo "DNS not available (OK for airgapped)"`},
	}

	for i, step := range steps {
		t.Logf("--- Step: %s", step.name)

		execID := fmt.Sprintf("step-%d-%d", i, time.Now().UnixNano())
		pspec := &ocispec.Process{
			Args: []string{"/bin/bash", "-e", "-c", step.cmd},
			Env:  envVars,
			Cwd:  "/home/runner/_work/repo/repo",
			User: ocispec.User{UID: 0, GID: 0},
		}

		process, err := task.Exec(nsCtx, execID, pspec, cio.NewCreator(cio.WithStdio))
		if err != nil {
			t.Fatalf("exec step %q: %v", step.name, err)
		}

		exitCh, err := process.Wait(nsCtx)
		if err != nil {
			process.Delete(nsCtx, client.WithProcessKill)
			t.Fatalf("wait step %q: %v", step.name, err)
		}

		if err := process.Start(nsCtx); err != nil {
			process.Delete(nsCtx, client.WithProcessKill)
			t.Fatalf("start step %q: %v", step.name, err)
		}

		select {
		case status := <-exitCh:
			process.Delete(nsCtx)
			if status.ExitCode() != 0 {
				t.Fatalf("step %q failed with exit code %d", step.name, status.ExitCode())
			}
			t.Logf("    passed")
		case <-ctx.Done():
			process.Delete(nsCtx, client.WithProcessKill)
			t.Fatalf("step %q timed out", step.name)
		}
	}

	t.Logf("==> All workflow steps passed")
}

func TestE2E_ServeRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ctrdClient := sharedCtrd.Client()

	// Set up networking with a unique data dir to avoid bridge collisions
	netDataDir := filepath.Join(sharedDataDir, "net-serve")
	os.MkdirAll(netDataDir, 0o755)

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

	// Pull a lightweight image for testing
	nsCtx := namespaces.WithNamespace(ctx, e2eNamespace)
	testImage := "docker.io/library/alpine:latest"

	sharedLog.Info("pulling test image", "ref", testImage)
	if _, err := ctrdClient.GetImage(nsCtx, testImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, testImage, client.WithPullUnpack); err != nil {
			t.Fatalf("pulling image: %v", err)
		}
	}

	img, err := ctrdClient.GetImage(nsCtx, testImage)
	if err != nil {
		t.Fatalf("getting image: %v", err)
	}

	// Create a container with a simple entrypoint
	containerID := fmt.Sprintf("e2e-serve-%d", time.Now().UnixNano())

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithProcessArgs("sh", "-c", `echo "e2e serve test passed" && cat /etc/resolv.conf && uname -a`),
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

	// Create task with log capture
	logDir := filepath.Join(netDataDir, "logs")
	os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, containerID+".log")

	task, err := container.NewTask(nsCtx, cio.LogFile(logPath))
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
		if _, err := task.Delete(nsCtx, client.WithProcessKill); err != nil {
			t.Logf("failed to delete task: %v", err)
		}
	}()

	// Attach networking
	pid := task.Pid()
	netns := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := net.Setup(ctx, containerID, netns); err != nil {
		t.Fatalf("setting up network: %v", err)
	}
	defer func() {
		if err := net.Teardown(ctx, containerID, netns); err != nil {
			t.Logf("failed to teardown network: %v", err)
		}
	}()

	// Start and wait
	if err := task.Start(nsCtx); err != nil {
		t.Fatalf("starting task: %v", err)
	}

	exitCh, err := task.Wait(nsCtx)
	if err != nil {
		t.Fatalf("waiting for task: %v", err)
	}

	select {
	case status := <-exitCh:
		if err := status.Error(); err != nil {
			t.Fatalf("task exited with error: %v", err)
		}
		if status.ExitCode() != 0 {
			logData, _ := os.ReadFile(logPath)
			t.Fatalf("task exited with code %d, logs:\n%s", status.ExitCode(), string(logData))
		}
		t.Logf("container exited with code 0")
	case <-ctx.Done():
		t.Fatalf("timed out waiting for container")
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	t.Logf("container output:\n%s", string(logData))
	if len(logData) == 0 {
		t.Error("expected non-empty log output")
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (no go.mod found)")
		}
		dir = parent
	}
}

func withDNSMount(dataDir string, containerID string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		content := buildResolvConf()
		dir := filepath.Join(dataDir, "dns")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating dns dir: %w", err)
		}
		src := filepath.Join(dir, containerID+".conf")
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing resolv.conf: %w", err)
		}
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      src,
			Options:     []string{"rbind", "ro"},
		})
		return nil
	}
}

func buildResolvConf() string {
	hostConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	}
	var lines []string
	hasNameserver := false
	for _, line := range strings.Split(string(hostConf), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "nameserver") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 && isRoutableDNS(parts[1]) {
				lines = append(lines, trimmed)
				hasNameserver = true
			}
		} else if strings.HasPrefix(trimmed, "search") || strings.HasPrefix(trimmed, "options") {
			lines = append(lines, trimmed)
		}
	}
	if !hasNameserver {
		lines = append([]string{"nameserver 1.1.1.1", "nameserver 8.8.8.8"}, lines...)
	}
	return strings.Join(lines, "\n") + "\n"
}

func withBindMount(hostPath, containerPath string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: containerPath,
			Type:        "bind",
			Source:      hostPath,
			Options:     []string{"rbind", "rw"},
		})
		return nil
	}
}

type gitInfo struct {
	SHA        string
	Ref        string
	Repository string
}

func sniffGitInfo(dir string) gitInfo {
	gi := gitInfo{SHA: "unknown", Ref: "refs/heads/main", Repository: "local/repo"}
	if out, err := gitCmd(dir, "rev-parse", "HEAD"); err == nil {
		gi.SHA = strings.TrimSpace(out)
	}
	if out, err := gitCmd(dir, "symbolic-ref", "HEAD"); err == nil {
		gi.Ref = strings.TrimSpace(out)
	}
	if out, err := gitCmd(dir, "remote", "get-url", "origin"); err == nil {
		gi.Repository = parseRepoFromURL(strings.TrimSpace(out))
	}
	return gi
}

func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func parseRepoFromURL(url string) string {
	if strings.Contains(url, ":") && strings.HasPrefix(url, "git@") {
		parts := strings.SplitN(url, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	url = strings.TrimSuffix(url, ".git")
	parts := strings.Split(url, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return url
}

func isRoutableDNS(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return true
	}
	switch parts[0] {
	case "10", "169", "127":
		return false
	case "172":
		second := 0
		if _, err := fmt.Sscanf(parts[1], "%d", &second); err != nil {
			return true
		}
		return second < 16 || second > 31
	case "192":
		return parts[1] != "168"
	}
	return true
}
