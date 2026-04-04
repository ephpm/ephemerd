package runtime

import (
	"context"
	"fmt"
	"log/slog"
	goruntime "runtime"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

const namespace = "ephemerd"

// Config for the container runtime.
type Config struct {
	Client       *client.Client
	DefaultImage string
	RunnerDir    string // host path to extracted runner binary
	RunnerMount  string // container path to mount runner at
	Log          *slog.Logger
}

// Runtime manages container lifecycle for runner environments.
type Runtime struct {
	cfg    Config
	client *client.Client
}

// RunnerEnv represents a running runner environment.
type RunnerEnv struct {
	ID        string
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

// PullImage ensures the runner image is available locally.
func (r *Runtime) PullImage(ctx context.Context, ref string) error {
	ctx = namespaces.WithNamespace(ctx, namespace)

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

	if image == "" {
		image = r.cfg.DefaultImage
	}

	r.cfg.Log.Info("creating runner environment", "id", id, "image", image)

	// Get the image
	img, err := r.client.GetImage(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("getting image %s: %w", image, err)
	}

	// Determine runner entrypoint based on OS
	entrypoint := "/actions-runner/run.sh"
	if goruntime.GOOS == "windows" {
		entrypoint = `C:\actions-runner\run.cmd`
	}

	// Build container spec
	opts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv([]string{
			"RUNNER_ALLOW_RUNASROOT=1",
		}),
		// Override entrypoint to run the injected runner with JIT config
		oci.WithProcessArgs(entrypoint, "--jitconfig", jitConfig),
	}

	// Bind-mount the runner directory into the container
	if r.cfg.RunnerDir != "" && r.cfg.RunnerMount != "" {
		opts = append(opts, withRunnerMount(r.cfg.RunnerDir, r.cfg.RunnerMount))
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

	// Create and start the task
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		container.Delete(ctx)
		return nil, fmt.Errorf("creating task for %s: %w", id, err)
	}

	if err := task.Start(ctx); err != nil {
		task.Delete(ctx)
		container.Delete(ctx)
		return nil, fmt.Errorf("starting task for %s: %w", id, err)
	}

	r.cfg.Log.Info("runner environment started", "id", id)

	return &RunnerEnv{
		ID:        id,
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
		env.Task.Wait(ctx)
	}

	// Delete task
	if _, err := env.Task.Delete(ctx); err != nil {
		r.cfg.Log.Warn("failed to delete task", "id", env.ID, "error", err)
	}

	// Delete container and snapshot
	if err := env.Container.Delete(ctx, client.WithSnapshotCleanup); err != nil {
		r.cfg.Log.Warn("failed to delete container", "id", env.ID, "error", err)
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

// withRunnerMount bind-mounts the host runner directory into the container as read-only.
func withRunnerMount(hostDir, containerDir string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Mounts == nil {
			s.Mounts = []ocispec.Mount{}
		}
		s.Mounts = append(s.Mounts, ocispec.Mount{
			Destination: containerDir,
			Type:        "bind",
			Source:      hostDir,
			Options:     []string{"rbind", "ro"},
		})
		return nil
	}
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
