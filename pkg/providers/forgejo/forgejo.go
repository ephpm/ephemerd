// Package forgejo implements providers.Provider for Forgejo Actions.
//
// Forgejo Actions is API-compatible with GitHub Actions workflow syntax
// but uses a different runner registration and job discovery model.
// Runners are persistent (not JIT) and poll for tasks via the FetchTask
// protocol defined in forgejo/runner-proto.
//
// Integration strategy:
//   - Register a runner with the Forgejo instance via /api/v1/runners/registration
//   - Poll for tasks via the runner-proto FetchTask/UpdateTask/UpdateLog protocol
//   - Embed the forgejo-runner binary in the container (same pattern as GHA runner)
//   - forgejo-runner executes in "host" mode inside ephemerd's container boundary
//
// Reference:
//   - Runner source: https://code.forgejo.org/forgejo/runner
//   - Runner proto: https://code.forgejo.org/forgejo/runner-proto
//   - API docs: https://forgejo.org/docs/next/user/actions/
package forgejo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/providers"
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

	Log *slog.Logger
}

// Provider implements providers.Provider for Forgejo Actions.
type Provider struct {
	cfg    Config
	events chan providers.JobEvent
	cancel context.CancelFunc

	// runnerToken is the persistent token received after registration.
	// Used for all subsequent FetchTask calls.
	runnerToken string
	runnerID    int64
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

func (p *Provider) Name() string { return "forgejo" }

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Register this runner with the Forgejo instance.
	// POST /api/v1/runners/registration with the registration token.
	// The instance returns a persistent runner token for task polling.
	if err := p.register(ctx); err != nil {
		return nil, fmt.Errorf("forgejo runner registration: %w", err)
	}

	p.cfg.Log.Info("forgejo runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
	)

	// Start polling for tasks via the runner-proto FetchTask protocol.
	go p.pollLoop(ctx, cfg.PollInterval)

	return p.events, nil
}

func (p *Provider) register(ctx context.Context) error {
	// TODO: implement runner registration
	// POST {instance_url}/api/v1/runners/registration
	// Body: { "token": p.cfg.Token }
	// Response: { "id": 123, "token": "runner-token-xxx" }
	return fmt.Errorf("forgejo: runner registration not yet implemented")
}

func (p *Provider) pollLoop(ctx context.Context, intervalSec int) {
	// TODO: implement FetchTask polling
	// POST {instance_url}/api/actions/runner/fetch-task
	// Auth: Bearer {runner_token}
	// Response: task proto with job details
	//
	// On receiving a task:
	//   1. Convert to providers.JobEvent
	//   2. Send to p.events channel
	//
	// The runner-proto protocol also requires:
	//   - UpdateTask: report job status back to Forgejo
	//   - UpdateLog: stream job logs back to Forgejo
	// These will be called from within the container by forgejo-runner.
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	// Forgejo runners are persistent — no per-job registration needed.
	// The runner token and instance URL are injected into the container
	// as environment variables. forgejo-runner handles the rest.
	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"FORGEJO_URL":          p.cfg.InstanceURL,
			"FORGEJO_RUNNER_TOKEN": p.runnerToken,
		},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	// TODO: report job completion back to Forgejo if needed.
	// forgejo-runner handles this via UpdateTask, so this may be a no-op.
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	// TODO: parse workflow YAML for EPHEMERD_IMAGE, similar to GitHub.
	// Forgejo's task proto may include the workflow content directly,
	// or we may need to fetch it via the Forgejo API:
	//   GET /api/v1/repos/{owner}/{repo}/contents/{path}
	return ""
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	// TODO: unregister runner from Forgejo instance
	// DELETE /api/v1/runners/{runner_id}
	return nil
}
