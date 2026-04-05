package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	goruntime "runtime"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/networking"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	namespace    = "ephemerd"
	defaultImage = "ghcr.io/actions/actions-runner:latest"
)

// Config for the container runtime.
type Config struct {
	Client       *client.Client
	RunnerDir    string // host path to extracted runner binary
	RunnerMount  string // container path to mount runner at
	LogDir       string // directory for per-job container logs
	Network      *networking.Manager
	Log          *slog.Logger
}

// Runtime manages container lifecycle for runner environments.
type Runtime struct {
	cfg    Config
	client *client.Client
	pullMu sync.Mutex // serializes image pulls to avoid content store contention
}

// RunnerEnv represents a running runner environment.
type RunnerEnv struct {
	ID        string
	Netns     string // network namespace path (Linux only)
	RunnerDir string // per-job runner copy, cleaned up on destroy
	Container client.Container
	Task      client.Task
}

// New creates a container runtime manager.
func New(cfg Config) (*Runtime, error) {
	return &Runtime{
		cfg:    cfg,
		client: cfg.Client,
	}, nil
}

// CleanOrphans removes any leftover containers from a previous ephemerd run.
// This should be called on startup before the scheduler starts accepting jobs.
func (r *Runtime) CleanOrphans(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	containers, err := r.client.Containers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	if len(containers) == 0 {
		return nil
	}

	r.cfg.Log.Info("cleaning orphan containers", "count", len(containers))

	for _, c := range containers {
		id := c.ID()
		log := r.cfg.Log.With("id", id)

		// Try to kill and delete the task
		task, err := c.Task(ctx, nil)
		if err == nil {
			status, err := task.Status(ctx)
			if err == nil && status.Status == client.Running {
				log.Debug("killing orphan task")
				_ = task.Kill(ctx, 9)
				exitCh, err := task.Wait(ctx)
				if err == nil {
					<-exitCh
				}
			}
			_, _ = task.Delete(ctx)
		}

		// Delete container and snapshot
		if err := c.Delete(ctx, client.WithSnapshotCleanup); err != nil {
			log.Warn("failed to delete orphan container", "error", err)
		} else {
			log.Info("orphan container removed")
		}
	}

	return nil
}

// PullImage ensures the runner image is available locally.
// Serialized with a mutex to avoid concurrent pulls contending on
// the content store (which produces noisy lock errors).
func (r *Runtime) PullImage(ctx context.Context, ref string) error {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()

	ctx = namespaces.WithNamespace(ctx, namespace)

	// Check if another goroutine already pulled it while we waited
	if _, err := r.client.GetImage(ctx, ref); err == nil {
		return nil
	}

	r.cfg.Log.Info("pulling image", "ref", ref)

	_, err := r.client.Pull(ctx, ref,
		client.WithPullUnpack,
	)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}

	r.cfg.Log.Info("image ready", "ref", ref)
	return nil
}

// Create provisions an ephemeral runner environment.
func (r *Runtime) Create(ctx context.Context, id string, image string, jitConfig string) (*RunnerEnv, error) {
	ctx = namespaces.WithNamespace(ctx, namespace)

	// Use the official GitHub Actions runner image when no custom image is specified.
	// The official image has the runner binary pre-installed at /home/runner/.
	// Custom images get our embedded runner binary mounted in.
	customImage := image != ""
	if !customImage {
		image = defaultImage
	}

	r.cfg.Log.Info("creating runner environment", "id", id, "image", image, "custom", customImage)

	// Get the image, pulling it if not present locally
	img, err := r.client.GetImage(ctx, image)
	if err != nil {
		r.cfg.Log.Info("image not found locally, pulling", "image", image)
		if err := r.PullImage(ctx, image); err != nil {
			return nil, fmt.Errorf("pulling image %s: %w", image, err)
		}
		img, err = r.client.GetImage(ctx, image)
		if err != nil {
			return nil, fmt.Errorf("getting image %s after pull: %w", image, err)
		}
	}

	// Runner paths differ: official image has runner at /home/runner,
	// custom images get our embedded runner mounted at /actions-runner.
	var entrypoint string
	if goruntime.GOOS == "windows" {
		entrypoint = `C:\actions-runner\run.cmd`
	} else if customImage {
		entrypoint = "/actions-runner/run.sh"
	} else {
		entrypoint = "/home/runner/run.sh"
	}

	// Build container spec
	opts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv([]string{
			"RUNNER_ALLOW_RUNASROOT=1",
		}),
		oci.WithProcessArgs(entrypoint, "--jitconfig", jitConfig),
	}

	// Only mount our embedded runner for custom images — the official image
	// already has the runner binary built in.
	var jobRunnerDir string
	if customImage && r.cfg.RunnerDir != "" && r.cfg.RunnerMount != "" {
		jobRunnerDir = filepath.Join(filepath.Dir(r.cfg.RunnerDir), "job-"+id)
		if err := copyDirHardlink(r.cfg.RunnerDir, jobRunnerDir); err != nil {
			return nil, fmt.Errorf("copying runner dir for %s: %w", id, err)
		}
		opts = append(opts, withRunnerMount(jobRunnerDir, r.cfg.RunnerMount))
	}

	// Mount host DNS config so containers can resolve names
	if goruntime.GOOS != "windows" {
		opts = append(opts, withDNSMount(filepath.Dir(r.cfg.LogDir), id))
	}

	// Add Hyper-V isolation on Windows
	if goruntime.GOOS == "windows" {
		opts = append(opts, withHyperVIsolation())
	}

	container, err := r.client.NewContainer(ctx, id,
		client.WithImage(img),
		client.WithNewSnapshot(id+"-snapshot", img),
		client.WithNewSpec(opts...),
	)
	if err != nil {
		return nil, fmt.Errorf("creating container %s: %w", id, err)
	}

	// Create and start the task with per-job log capture
	var creator cio.Creator
	if r.cfg.LogDir != "" {
		_ = os.MkdirAll(r.cfg.LogDir, 0o755)
		logPath := filepath.Join(r.cfg.LogDir, id+".log")
		creator = cio.LogFile(logPath)
		r.cfg.Log.Debug("container logs", "id", id, "path", logPath)
	} else {
		creator = cio.NewCreator(cio.WithStdio)
	}
	task, err := container.NewTask(ctx, creator)
	if err != nil {
		_ = container.Delete(ctx)
		return nil, fmt.Errorf("creating task for %s: %w", id, err)
	}

	// Attach CNI networking before starting the task
	var netns string
	if r.cfg.Network != nil && goruntime.GOOS != "windows" {
		pid := task.Pid()
		netns = fmt.Sprintf("/proc/%d/ns/net", pid)
		if _, err := r.cfg.Network.Setup(ctx, id, netns); err != nil {
			_, _ = task.Delete(ctx)
			_ = container.Delete(ctx)
			return nil, fmt.Errorf("setting up network for %s: %w", id, err)
		}
	}

	if err := task.Start(ctx); err != nil {
		if r.cfg.Network != nil && netns != "" {
			_ = r.cfg.Network.Teardown(ctx, id, netns)
		}
		_, _ = task.Delete(ctx)
		_ = container.Delete(ctx)
		return nil, fmt.Errorf("starting task for %s: %w", id, err)
	}

	r.cfg.Log.Info("runner environment started", "id", id)

	return &RunnerEnv{
		ID:        id,
		Netns:     netns,
		RunnerDir: jobRunnerDir,
		Container: container,
		Task:      task,
	}, nil
}

// Destroy tears down a runner environment completely.
func (r *Runtime) Destroy(ctx context.Context, env *RunnerEnv) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

	r.cfg.Log.Info("destroying runner environment", "id", env.ID)

	// Kill the task if still running
	status, err := env.Task.Status(ctx)
	if err == nil && status.Status == client.Running {
		if err := env.Task.Kill(ctx, 9); err != nil {
			r.cfg.Log.Warn("failed to kill task", "id", env.ID, "error", err)
		}
		exitCh, err := env.Task.Wait(ctx)
		if err == nil {
			<-exitCh
		}
	}

	// Delete task
	if _, err := env.Task.Delete(ctx); err != nil {
		r.cfg.Log.Warn("failed to delete task", "id", env.ID, "error", err)
	}

	// Teardown CNI networking
	if r.cfg.Network != nil && env.Netns != "" {
		if err := r.cfg.Network.Teardown(ctx, env.ID, env.Netns); err != nil {
			r.cfg.Log.Warn("failed to teardown network", "id", env.ID, "error", err)
		}
	}

	// Delete container and snapshot
	if err := env.Container.Delete(ctx, client.WithSnapshotCleanup); err != nil {
		r.cfg.Log.Warn("failed to delete container", "id", env.ID, "error", err)
	}

	// Clean up per-job runner directory copy
	if env.RunnerDir != "" {
		if err := os.RemoveAll(env.RunnerDir); err != nil {
			r.cfg.Log.Warn("failed to remove job runner dir", "id", env.ID, "path", env.RunnerDir, "error", err)
		}
	}

	r.cfg.Log.Info("runner environment destroyed", "id", env.ID)
	return nil
}

// Wait blocks until the runner environment's task exits.
// Returns the exit status code.
func (r *Runtime) Wait(ctx context.Context, env *RunnerEnv) (uint32, error) {
	ctx = namespaces.WithNamespace(ctx, namespace)

	exitCh, err := env.Task.Wait(ctx)
	if err != nil {
		return 1, fmt.Errorf("waiting for task %s: %w", env.ID, err)
	}

	select {
	case status := <-exitCh:
		return status.ExitCode(), status.Error()
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

// withDNSMount creates a resolv.conf for the container.
// We write a temporary file with the host's nameservers, filtering out
// any private/unreachable IPs (e.g. WSL2's 10.255.255.254) and falling
// back to public DNS if no usable nameservers are found.
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

// buildResolvConf reads the host's resolv.conf and filters out private
// nameservers that containers can't reach. Falls back to public DNS.
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
			// Extract the IP and check if it's routable from containers
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

// isRoutableDNS checks if a DNS server IP is reachable from containers.
// Private IPs (10.x, 172.16-31.x, 192.168.x, 169.254.x) are blocked
// by our firewall rules, so we filter them out.
func isRoutableDNS(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return true // IPv6 or weird format, let it through
	}
	first := parts[0]
	switch first {
	case "10", "169":
		return false
	case "172":
		// 172.16.0.0/12
		second := 0
		if _, err := fmt.Sscanf(parts[1], "%d", &second); err != nil {
			return true // can't parse, assume routable
		}
		return second < 16 || second > 31
	case "192":
		return parts[1] != "168"
	case "127":
		return false
	}
	return true
}

// withRunnerMount bind-mounts a per-job copy of the runner directory into the container.
// The runner needs write access (e.g. run-helper.sh at startup) so we can't use
// the shared extracted dir directly. The caller provides a job-specific copy.
func withRunnerMount(hostDir, containerDir string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: containerDir,
			Type:        "bind",
			Source:      hostDir,
			Options:     []string{"rbind", "rw"},
		})
		return nil
	}
}

// copyDirHardlink creates a copy of src at dst using hardlinks (cp -al).
// This is instant and uses no extra disk space until files are modified.
func copyDirHardlink(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return exec.Command("cp", "-al", src, dst).Run()
}

// withHyperVIsolation is a spec option that enables Hyper-V isolation on Windows.
func withHyperVIsolation() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Windows == nil {
			s.Windows = &ocispec.Windows{}
		}
		s.Windows.HyperV = &ocispec.WindowsHyperV{}
		return nil
	}
}
