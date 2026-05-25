package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/cni"
	ctdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/networking"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	namespace    = "ephemerd"
	defaultImage = "ghcr.io/actions/actions-runner:latest"
)

// Runner executes workflow jobs locally using embedded containerd.
type Runner struct {
	DataDir    string
	SocketPath string // optional: containerd socket override for isolation from the service
	Log        *slog.Logger
}

// gitInfo holds repository metadata sniffed from the local git repo.
type gitInfo struct {
	SHA        string
	Ref        string
	Repository string // owner/repo
}

// RunJob executes a single workflow job inside a container.
func (r *Runner) RunJob(ctx context.Context, jobName string, job Job, repoDir string) error {
	r.Log.Info("starting local job execution", "job", jobName)
	fmt.Printf("\n==> Job: %s\n", jobName)

	// Sniff git info from the repo directory
	gi := sniffGitInfo(repoDir)

	// Start embedded containerd
	r.Log.Info("starting containerd")
	ctrd, err := ctdpkg.New(ctdpkg.Config{
		DataDir:    r.DataDir,
		SocketPath: r.SocketPath,
		Log:        r.Log,
	})
	if err != nil {
		return fmt.Errorf("starting containerd: %w", err)
	}
	defer ctrd.Stop()

	ctrdClient := ctrd.Client()

	// Extract CNI plugins
	cm := cni.New(r.DataDir, r.Log)
	if err := cm.Extract(); err != nil {
		return fmt.Errorf("extracting CNI plugins: %w", err)
	}

	// Initialize networking
	net, err := networking.New(networking.Config{
		DataDir:   r.DataDir,
		CNIBinDir: cm.Dir(),
		Log:       r.Log,
	})
	if err != nil {
		return fmt.Errorf("initializing networking: %w", err)
	}
	defer net.Cleanup()

	if err := net.InstallFirewallRules(); err != nil {
		r.Log.Warn("failed to install firewall rules", "error", err)
	}

	// Pull the runner image
	nsCtx := namespaces.WithNamespace(ctx, namespace)

	r.Log.Info("pulling image", "ref", defaultImage)
	if _, err := ctrdClient.GetImage(nsCtx, defaultImage); err != nil {
		if _, err := ctrdClient.Pull(nsCtx, defaultImage, client.WithPullUnpack); err != nil {
			return fmt.Errorf("pulling image %s: %w", defaultImage, err)
		}
	}

	img, err := ctrdClient.GetImage(nsCtx, defaultImage)
	if err != nil {
		return fmt.Errorf("getting image: %w", err)
	}

	// Build environment variables
	envVars := []string{
		"RUNNER_ALLOW_RUNASROOT=1",
		"GITHUB_WORKSPACE=/home/runner/_work/repo/repo",
		fmt.Sprintf("GITHUB_SHA=%s", gi.SHA),
		fmt.Sprintf("GITHUB_REF=%s", gi.Ref),
		fmt.Sprintf("GITHUB_REPOSITORY=%s", gi.Repository),
		"GITHUB_ACTIONS=true",
		"CI=true",
	}

	// Add job-level environment variables
	for k, v := range job.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
	}

	// Build container spec: use sleep to keep the container alive while we exec steps
	containerID := fmt.Sprintf("ephemerd-run-%d", time.Now().UnixNano())
	snapshotName := containerID + "-snapshot"

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv(envVars),
		oci.WithProcessArgs("sleep", "86400"),
		withBindMount(repoDir, "/home/runner/_work/repo/repo"),
	}

	// Clean up stale snapshot if exists
	snapshotter := ctrdClient.SnapshotService("overlayfs")
	if snapshotter != nil {
		if _, statErr := snapshotter.Stat(nsCtx, snapshotName); statErr == nil {
			_ = snapshotter.Remove(nsCtx, snapshotName)
		}
	}

	container, err := ctrdClient.NewContainer(nsCtx, containerID,
		client.WithImage(img),
		client.WithNewSnapshot(snapshotName, img),
		client.WithNewSpec(specOpts...),
	)
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}
	defer func() {
		_ = container.Delete(nsCtx, client.WithSnapshotCleanup)
	}()

	// Create and start the task
	task, err := container.NewTask(nsCtx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("creating task: %w", err)
	}
	defer func() {
		if status, err := task.Status(nsCtx); err == nil && status.Status == client.Running {
			_ = task.Kill(nsCtx, 9)
			exitCh, err := task.Wait(nsCtx)
			if err == nil {
				<-exitCh
			}
		}
		_, _ = task.Delete(nsCtx, client.WithProcessKill)
	}()

	// Attach networking
	pid := task.Pid()
	netns := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := net.Setup(ctx, containerID, netns); err != nil {
		return fmt.Errorf("setting up network: %w", err)
	}
	defer func() {
		_ = net.Teardown(ctx, containerID, netns)
	}()

	if err := task.Start(nsCtx); err != nil {
		return fmt.Errorf("starting task: %w", err)
	}

	r.Log.Info("container started", "id", containerID)

	// Execute each step
	var failed bool
	for i, step := range job.Steps {
		stepName := step.Name
		if stepName == "" {
			stepName = fmt.Sprintf("Step %d", i+1)
		}

		if step.Uses != "" {
			fmt.Printf("--- Step: %s (uses)\n", stepName)
			fmt.Printf("    skipped (action execution not yet supported: %s)\n", step.Uses)
			continue
		}

		if step.Run == "" {
			continue
		}

		fmt.Printf("--- Step: %s (run)\n", stepName)

		start := time.Now()
		exitCode, err := r.execStep(nsCtx, task, step, envVars)
		elapsed := time.Since(start)

		if err != nil {
			fmt.Printf("    FAILED (%s) error: %v\n", elapsed.Truncate(time.Millisecond), err)
			failed = true
			break
		}

		if exitCode != 0 {
			fmt.Printf("    FAILED (%s) exit code: %d\n", elapsed.Truncate(time.Millisecond), exitCode)
			failed = true
			break
		}

		fmt.Printf("    passed (%s)\n", elapsed.Truncate(time.Millisecond))
	}

	if failed {
		return fmt.Errorf("job %q failed", jobName)
	}

	fmt.Printf("\n==> Job %s completed successfully\n", jobName)
	return nil
}

// execStep runs a single shell command inside the container, streaming output to stdout/stderr.
func (r *Runner) execStep(ctx context.Context, task client.Task, step Step, baseEnv []string) (uint32, error) {
	// Build step-specific environment
	env := make([]string, len(baseEnv))
	copy(env, baseEnv)
	for k, v := range step.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execID := fmt.Sprintf("step-%d", time.Now().UnixNano())

	pspec := &ocispec.Process{
		Args: []string{"/bin/bash", "-e", "-c", step.Run},
		Env:  env,
		Cwd:  "/home/runner/_work/repo/repo",
		User: ocispec.User{
			UID: 0,
			GID: 0,
		},
	}

	process, err := task.Exec(ctx, execID, pspec, cio.NewCreator(cio.WithStdio))
	if err != nil {
		return 1, fmt.Errorf("exec: %w", err)
	}
	defer func() {
		_, _ = process.Delete(ctx, client.WithProcessKill)
	}()

	exitCh, err := process.Wait(ctx)
	if err != nil {
		return 1, fmt.Errorf("waiting for exec: %w", err)
	}

	if err := process.Start(ctx); err != nil {
		return 1, fmt.Errorf("starting exec: %w", err)
	}

	select {
	case status := <-exitCh:
		if err := status.Error(); err != nil {
			return 1, err
		}
		return status.ExitCode(), nil
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

// withBindMount creates an OCI spec option that bind-mounts a host path into the container.
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

// sniffGitInfo extracts git metadata from the current repo directory.
func sniffGitInfo(dir string) gitInfo {
	gi := gitInfo{
		SHA:        "unknown",
		Ref:        "refs/heads/main",
		Repository: "local/repo",
	}

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

// gitCmd runs a git command in the given directory and returns its stdout.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseRepoFromURL extracts "owner/repo" from a git remote URL.
// Handles both HTTPS (https://github.com/owner/repo.git) and
// SSH (git@github.com:owner/repo.git) formats.
func parseRepoFromURL(url string) string {
	// SSH format: git@github.com:owner/repo.git
	if strings.Contains(url, ":") && strings.HasPrefix(url, "git@") {
		parts := strings.SplitN(url, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}

	// HTTPS format: https://github.com/owner/repo.git
	url = strings.TrimSuffix(url, ".git")
	parts := strings.Split(url, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}

	return url
}
