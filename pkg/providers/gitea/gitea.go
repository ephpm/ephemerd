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

	// JobImage is the default OCI image for job execution containers.
	// The runner daemon creates job containers via the fake Docker socket;
	// this image is what those containers run.
	// Default: "docker.io/gitea/runner-images:ubuntu-24.04"
	JobImage string

	Log *slog.Logger
}

// Provider implements providers.Provider for Gitea Actions.
type Provider struct {
	cfg    Config
	events chan providers.JobEvent
	cancel context.CancelFunc

	// runnerToken is the persistent token received after registration.
	// Passed into containers for act_runner to use.
	runnerToken string
	runnerID    int64
}

// New creates a Gitea provider.
func New(cfg Config) (*Provider, error) {
	if cfg.InstanceURL == "" {
		return nil, fmt.Errorf("gitea: instance_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gitea: token is required")
	}
	return &Provider{
		cfg:    cfg,
		events: make(chan providers.JobEvent, 64),
	}, nil
}

func (p *Provider) Name() string            { return "gitea" }
func (p *Provider) DefaultImage() string    { return defaultImage }
func (p *Provider) DefaultJobImage() string {
	if p.cfg.JobImage != "" {
		return p.cfg.JobImage
	}
	return defaultJobImage
}

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Register this runner with the Gitea instance.
	// Uses the ConnectRPC Register endpoint to exchange the registration
	// token for a persistent runner ID + auth token.
	if err := p.register(ctx); err != nil {
		return nil, fmt.Errorf("gitea runner registration: %w", err)
	}

	p.cfg.Log.Info("gitea runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
	)

	// Poll for tasks via ConnectRPC FetchTask.
	// When a task arrives, emit a JobEvent so the scheduler can spin
	// up a container and launch act_runner --ephemeral inside it.
	go p.pollLoop(ctx, cfg.PollInterval)

	return p.events, nil
}

func (p *Provider) register(ctx context.Context) error {
	// TODO: ConnectRPC call to RunnerService/Register
	//
	// Request:  { name, token, labels, version }
	// Response: { id, token }  (persistent runner credentials)
	//
	// Proto package: code.gitea.io/actions-proto-go
	//
	// Store p.runnerID and p.runnerToken from response.
	// Then call RunnerService/Declare to announce labels.
	return fmt.Errorf("gitea: runner registration not yet implemented")
}

func (p *Provider) pollLoop(ctx context.Context, intervalSec int) {
	// TODO: ConnectRPC call to RunnerService/FetchTask
	//
	// FetchTask returns a Task proto containing:
	//   - task ID
	//   - workflow_payload (YAML bytes)
	//   - context (repo, ref, secrets, etc.)
	//
	// On receiving a task, convert to providers.JobEvent and send on
	// p.events. The scheduler will launch a container with act_runner
	// in --ephemeral mode which picks up the task.
	//
	// FetchTask supports long-poll (~5s server timeout) with backoff.
	// tasks_version field enables change detection.
	//
	// Note: unlike Forgejo, Gitea's FetchTask returns a single task
	// per request (no multi-task batch support).
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	// No per-job registration needed — act_runner uses the persistent
	// runner credentials. In --ephemeral mode it runs one job and exits.
	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"GITEA_INSTANCE_URL": p.cfg.InstanceURL,
			"GITEA_RUNNER_TOKEN": p.runnerToken,
		},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	// act_runner handles UpdateTask/UpdateLog — nothing to clean up.
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	// TODO: the Task proto from FetchTask includes workflow_payload bytes.
	// Parse the YAML and look for EPHEMERD_IMAGE in env, same as GitHub.
	// If not available from the task, fall back to the Gitea API:
	//   GET /api/v1/repos/{owner}/{repo}/contents/{path}
	return ""
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	// TODO: unregister runner via ConnectRPC or REST API
	return nil
}
