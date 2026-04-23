// Package gitea implements providers.Provider for Gitea Actions.
//
// Gitea Actions uses the same workflow syntax as GitHub Actions but runs
// jobs via act_runner, which embeds nektos/act and talks to the Gitea
// instance over ConnectRPC (Register, Declare, FetchTask, UpdateTask,
// UpdateLog).
//
// Integration model (embed binary):
//
//	ephemerd polls for tasks via the ConnectRPC FetchTask endpoint.
//	When a job arrives, ephemerd spins up a container from the default
//	runner image (which has act_runner pre-installed) and launches:
//
//	  act_runner daemon --ephemeral
//
//	with the runner config pre-seeded (instance URL, token, labels).
//	The runner handles workflow execution, log streaming, and status
//	reporting. ephemerd manages the container lifecycle.
//
// Gitea vs Forgejo:
//
//	While the protocol is nearly identical (both ConnectRPC, same 5 RPCs),
//	the proto packages and runner binaries have diverged:
//	  - Gitea:   act_runner       / code.gitea.io/actions-proto-go
//	  - Forgejo: forgejo-runner   / code.forgejo.org/forgejo/actions-proto
//	  - Gitea uses --ephemeral flag; Forgejo uses dedicated one-job command
//
// Reference:
//   - Runner source: https://gitea.com/gitea/act_runner
//   - Runner proto:  https://gitea.com/gitea/actions-proto-def
//   - API docs:      https://docs.gitea.com/usage/actions/overview
package gitea

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
	"github.com/ephpm/ephemerd/pkg/names"
	"github.com/ephpm/ephemerd/pkg/providers"
)

const (
	defaultImage    = "docker.io/gitea/act_runner:latest"
	defaultJobImage = "docker.io/gitea/runner-images:ubuntu-24.04"
)

// Compile-time interface check.
var _ providers.Poll = (*Provider)(nil)

// Config for the Gitea provider.
type Config struct {
	// InstanceURL is the base URL of the Gitea instance (e.g., "https://gitea.example.com").
	InstanceURL string

	// Token is the runner registration token from the Gitea admin panel.
	// Found at: Site Administration > Actions > Runners > Create new runner.
	Token string

	// Owner is the organization or user that owns the runner.
	// If empty, the runner is registered at the instance level.
	Owner string

	// Repos limits the runner to specific repositories.
	// If empty, the runner accepts jobs from all repos the owner has access to.
	Repos []string

	// Labels are the runner labels to register with the forge.
	// Each label is a string like "ubuntu-latest:docker://image:tag".
	// If empty, defaults to ["ubuntu-latest:docker://<job_image>"].
	Labels []string

	// DefaultImage overrides the runner daemon container image.
	// Default: "docker.io/gitea/act_runner:latest"
	DefaultImage string

	// JobImage is the default OCI image for job execution containers.
	// The runner daemon creates job containers via the fake Docker socket;
	// this image is what those containers run.
	// Default: "docker.io/gitea/runner-images:ubuntu-24.04"
	JobImage string

	// HTTPClient is an optional *http.Client for the ConnectRPC client.
	// If nil, a default client with 30s timeout is used.
	HTTPClient *http.Client

	Log *slog.Logger
}

// Provider implements providers.Provider for Gitea Actions.
type Provider struct {
	cfg    Config
	rpc    *forgerpc.Client
	events chan providers.JobEvent
	cancel context.CancelFunc

	// Runner credentials from registration.
	runnerToken string
	runnerID    int64
	runnerUUID  string
}

// New creates a Gitea provider.
func New(cfg Config) (*Provider, error) {
	if cfg.InstanceURL == "" {
		return nil, fmt.Errorf("gitea: instance_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gitea: token is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Provider{
		cfg:    cfg,
		rpc:    forgerpc.NewClient(cfg.InstanceURL, cfg.HTTPClient),
		events: make(chan providers.JobEvent, 64),
	}, nil
}

func (p *Provider) Name() string         { return "gitea" }
func (p *Provider) DefaultImage() string {
	if p.cfg.DefaultImage != "" {
		return p.cfg.DefaultImage
	}
	return defaultImage
}
func (p *Provider) DefaultJobImage() string {
	if p.cfg.JobImage != "" {
		return p.cfg.JobImage
	}
	return defaultJobImage
}

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	if err := p.register(ctx); err != nil {
		return nil, fmt.Errorf("gitea runner registration: %w", err)
	}

	p.cfg.Log.Info("gitea runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
	)

	go p.pollLoop(ctx, cfg.PollInterval)

	return p.events, nil
}

func (p *Provider) register(ctx context.Context) error {
	runnerName := fmt.Sprintf("ephemerd-%s", names.Generate())
	labels := p.buildLabels()

	runner, err := p.rpc.Register(ctx, runnerName, p.cfg.Token, "ephemerd/v1", labels)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	p.runnerID = runner.ID
	p.runnerUUID = runner.UUID
	p.runnerToken = runner.Token

	if err := p.rpc.Declare(ctx, forgerpc.DeclareLabels(labels)); err != nil {
		p.cfg.Log.Warn("declare labels failed (non-fatal)", "error", err)
	}

	return nil
}

func (p *Provider) pollLoop(ctx context.Context, intervalSec int) {
	defer close(p.events)

	var tasksVersion int64
	var failCount int

	for {
		if ctx.Err() != nil {
			return
		}

		result, err := p.rpc.FetchTask(ctx, tasksVersion)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failCount++
			backoff := min(time.Duration(1<<uint(min(failCount, 6)))*time.Second, 60*time.Second)
			p.cfg.Log.Warn("fetch task failed", "error", err, "fail_count", failCount, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}

		failCount = 0
		tasksVersion = result.TasksVersion

		if result.Task == nil {
			delay := time.Duration(intervalSec) * time.Second
			if delay <= 0 {
				delay = 1 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}

		repo := result.Task.Repo()
		p.cfg.Log.Info("task received", "task_id", result.Task.ID, "repo", repo)

		select {
		case p.events <- providers.JobEvent{
			Action: "queued",
			Repo:   repo,
			JobID:  result.Task.ID,
			Raw:    result.Task,
		}:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	regLabels := strings.Join(p.buildLabels(), ",")
	regCmd := fmt.Sprintf(
		"act_runner register --no-interactive --instance $GITEA_INSTANCE_URL --token $GITEA_RUNNER_TOKEN --name %s --labels %s && act_runner daemon --ephemeral",
		runnerName, regLabels,
	)
	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"GITEA_INSTANCE_URL": p.cfg.InstanceURL,
			"GITEA_RUNNER_TOKEN": p.cfg.Token, // registration token — container self-registers
		},
		Entrypoint: []string{"sh", "-c", regCmd},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	task, ok := event.Raw.(*forgerpc.Task)
	if !ok || task == nil {
		return ""
	}
	return task.ContainerImage()
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

// buildLabels returns label strings for registration.
// Register uses repeated string; Declare converts to AgentLabel.
func (p *Provider) buildLabels() []string {
	if len(p.cfg.Labels) > 0 {
		return p.cfg.Labels
	}
	return []string{
		fmt.Sprintf("ubuntu-latest:docker://%s", p.DefaultJobImage()),
	}
}
