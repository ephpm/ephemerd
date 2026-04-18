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

	// JobImage is the default OCI image for job execution containers.
	// The runner daemon creates job containers via the fake Docker socket;
	// this image is what those containers run.
	// Default: "docker.io/gitea/runner-images:ubuntu-24.04"
	JobImage string

	Log *slog.Logger
}

// Provider implements providers.Provider for Forgejo Actions.
type Provider struct {
	cfg    Config
	events chan providers.JobEvent
	cancel context.CancelFunc

	// runnerToken is the persistent token received after registration.
	// Passed into containers for forgejo-runner to use.
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
	return &Provider{
		cfg:    cfg,
		events: make(chan providers.JobEvent, 64),
	}, nil
}

func (p *Provider) Name() string            { return "forgejo" }
func (p *Provider) DefaultImage() string    { return defaultImage }
func (p *Provider) DefaultJobImage() string {
	if p.cfg.JobImage != "" {
		return p.cfg.JobImage
	}
	return defaultJobImage
}

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Register this runner with the Forgejo instance.
	// Uses the ConnectRPC Register endpoint to exchange the registration
	// token for a persistent runner UUID + auth token.
	if err := p.register(ctx); err != nil {
		return nil, fmt.Errorf("forgejo runner registration: %w", err)
	}

	p.cfg.Log.Info("forgejo runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
		"runner_uuid", p.runnerUUID,
	)

	// Poll for tasks via ConnectRPC FetchTask.
	// When a task arrives, emit a JobEvent so the scheduler can spin
	// up a container and launch forgejo-runner one-job inside it.
	go p.pollLoop(ctx, cfg.PollInterval)

	return p.events, nil
}

func (p *Provider) register(ctx context.Context) error {
	// TODO: ConnectRPC call to RunnerService/Register
	//
	// Request:  { name, token, labels, version }
	// Response: { id, uuid, token }  (persistent runner credentials)
	//
	// Store p.runnerID, p.runnerUUID, p.runnerToken from response.
	// Then call RunnerService/Declare to announce labels.
	return fmt.Errorf("forgejo: runner registration not yet implemented")
}

func (p *Provider) pollLoop(ctx context.Context, intervalSec int) {
	// TODO: ConnectRPC call to RunnerService/FetchTask
	//
	// FetchTask returns a Task proto containing:
	//   - task ID and UUID
	//   - workflow_payload (YAML bytes)
	//   - context (repo, ref, secrets, etc.)
	//
	// On receiving a task, convert to providers.JobEvent and send on
	// p.events. The scheduler will launch a container with forgejo-runner
	// one-job --handle <task-uuid> which picks up the specific task.
	//
	// FetchTask supports long-poll (~5s server timeout) with backoff.
	// tasks_version field enables change detection.
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	// No per-job registration needed — forgejo-runner one-job uses
	// the persistent runner credentials to claim the specific task.
	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"FORGEJO_INSTANCE_URL": p.cfg.InstanceURL,
			"FORGEJO_RUNNER_TOKEN": p.runnerToken,
			"FORGEJO_RUNNER_UUID":  p.runnerUUID,
		},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	// forgejo-runner handles UpdateTask/UpdateLog — nothing to clean up.
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	// TODO: the Task proto from FetchTask includes workflow_payload bytes.
	// Parse the YAML and look for EPHEMERD_IMAGE in env, same as GitHub.
	// If not available from the task, fall back to the Forgejo API:
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
