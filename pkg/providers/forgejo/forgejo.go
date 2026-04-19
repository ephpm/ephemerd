// Package forgejo implements providers.Provider for Forgejo Actions.
//
// Forgejo Actions uses the same workflow syntax as GitHub Actions but runs
// jobs via forgejo-runner, a hard fork of Gitea's act_runner. The runner
// binary embeds a fork of nektos/act and talks to the Forgejo instance
// over ConnectRPC (Register, Declare, FetchTask, UpdateTask, UpdateLog).
//
// Integration model (embed binary):
//
//	ephemerd polls for tasks via the ConnectRPC FetchTask endpoint.
//	When a job arrives, ephemerd spins up a container from the default
//	runner image (which has forgejo-runner pre-installed) and launches:
//
//	  forgejo-runner one-job \
//	    --url <instance_url> \
//	    --token-url file:///run/secrets/token \
//	    --label <labels> \
//	    --handle <task-uuid>
//
//	The runner handles workflow execution, log streaming, and status
//	reporting. ephemerd manages the container lifecycle.
//
// Reference:
//   - Runner source: https://code.forgejo.org/forgejo/runner
//   - Runner proto:  https://code.forgejo.org/forgejo/actions-proto
//   - API docs:      https://forgejo.org/docs/next/user/actions/
package forgejo

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
	defaultImage    = "data.forgejo.org/forgejo/runner:12"
	defaultJobImage = "docker.io/gitea/runner-images:ubuntu-24.04"
)

// Compile-time interface check.
var _ providers.Poll = (*Provider)(nil)

// Config for the Forgejo provider.
type Config struct {
	// InstanceURL is the base URL of the Forgejo instance (e.g., "https://codeberg.org").
	InstanceURL string

	// Token is the runner registration token from the Forgejo admin panel.
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

// Provider implements providers.Provider for Forgejo Actions.
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

// New creates a Forgejo provider.
func New(cfg Config) (*Provider, error) {
	if cfg.InstanceURL == "" {
		return nil, fmt.Errorf("forgejo: instance_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("forgejo: token is required")
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

func (p *Provider) Name() string         { return "forgejo" }
func (p *Provider) DefaultImage() string { return defaultImage }
func (p *Provider) DefaultJobImage() string {
	if p.cfg.JobImage != "" {
		return p.cfg.JobImage
	}
	return defaultJobImage
}

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	if err := p.register(ctx); err != nil {
		return nil, fmt.Errorf("forgejo runner registration: %w", err)
	}

	p.cfg.Log.Info("forgejo runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
		"runner_uuid", p.runnerUUID,
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
		p.cfg.Log.Info("task received", "task_id", result.Task.ID, "task_uuid", result.Task.UUID, "repo", repo)

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
		"forgejo-runner register --no-interactive --instance $FORGEJO_INSTANCE_URL --token $FORGEJO_RUNNER_TOKEN --name %s --labels %s && forgejo-runner daemon",
		runnerName, regLabels,
	)
	env := map[string]string{
		"FORGEJO_INSTANCE_URL": p.cfg.InstanceURL,
		"FORGEJO_RUNNER_TOKEN": p.cfg.Token, // registration token — container self-registers
	}
	if task, ok := event.Raw.(*forgerpc.Task); ok && task != nil && task.UUID != "" {
		env["FORGEJO_TASK_UUID"] = task.UUID
	}

	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env:        env,
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
	return task.EphemerdImage()
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

func (p *Provider) buildLabels() []string {
	if len(p.cfg.Labels) > 0 {
		return p.cfg.Labels
	}
	return []string{
		fmt.Sprintf("ubuntu-latest:docker://%s", p.DefaultJobImage()),
	}
}
