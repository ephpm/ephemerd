// Package gitlab implements providers.Provider for GitLab CI.
//
// GitLab CI uses a fundamentally different runner model than GitHub:
//   - Runners are persistent and poll for jobs (no JIT registration)
//   - gitlab-runner supports a "custom executor" model where external
//     scripts handle prepare/run/cleanup — this is the integration path
//   - Job image is a first-class field in .gitlab-ci.yml (no YAML fetch needed)
//   - Tags instead of labels for runner matching
//   - Webhook verification uses a shared token header, not HMAC-SHA256
//
// Integration strategy (custom executor):
//   - ephemerd manages a gitlab-runner process in custom executor mode
//   - gitlab-runner handles registration, polling, and job protocol
//   - On job arrival, gitlab-runner calls ephemerd's prepare script:
//     ephemerd creates a container and returns the exec environment
//   - gitlab-runner calls run script: executes job steps in the container
//   - gitlab-runner calls cleanup script: ephemerd destroys the container
//
// This means ephemerd does NOT poll GitLab directly — gitlab-runner does.
// The custom executor scripts bridge gitlab-runner into ephemerd's container
// lifecycle.
//
// Reference:
//   - Custom executor docs: https://docs.gitlab.com/runner/executors/custom.html
//   - Runner registration API: https://docs.gitlab.com/ee/api/runners.html
//   - Job hooks: https://docs.gitlab.com/ee/administration/system_hooks.html
package gitlab

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/providers"
)

const defaultImage = "ghcr.io/ephpm/runner-gitlab:latest"

// Compile-time interface check.
var _ providers.Poll = (*Provider)(nil)

// Config for the GitLab provider.
type Config struct {
	// InstanceURL is the base URL of the GitLab instance (e.g., "https://gitlab.com").
	InstanceURL string

	// Token is the runner registration token from GitLab.
	// Found at: Settings > CI/CD > Runners > New project runner.
	// For GitLab 16+, this is a runner authentication token (glrt-xxx).
	Token string

	// Tags are the runner tags used for job matching in .gitlab-ci.yml.
	// GitLab dispatches jobs to runners whose tags match the job's tags: field.
	Tags []string

	// LinuxImage / WindowsImage override the runner image per job OS.
	// Empty values fall through to the built-in default
	// ("ghcr.io/ephpm/runner-gitlab:latest" for Linux) and (Windows) the
	// runtime's host-matched servercore default.
	LinuxImage   string
	WindowsImage string

	Log *slog.Logger
}

// Provider implements providers.Provider for GitLab CI.
type Provider struct {
	cfg    Config
	events chan providers.JobEvent
	cancel context.CancelFunc

	// runnerID is assigned by GitLab after registration.
	runnerID int64

	// runnerToken is the authentication token for this runner.
	// For GitLab 16+, this is the glrt-xxx token.
	// For older GitLab, this is returned by the registration API.
	runnerToken string
}

// New creates a GitLab provider.
func New(cfg Config) (*Provider, error) {
	if cfg.InstanceURL == "" {
		return nil, fmt.Errorf("gitlab: instance_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gitlab: token is required")
	}
	return &Provider{
		cfg:    cfg,
		events: make(chan providers.JobEvent, 64),
	}, nil
}

func (p *Provider) Name() string         { return "gitlab" }
func (p *Provider) DefaultImage() string { return p.DefaultImageFor("linux") }

// DefaultImageFor returns the runner image for the given job OS.
func (p *Provider) DefaultImageFor(os string) string {
	switch os {
	case "linux":
		if p.cfg.LinuxImage != "" {
			return p.cfg.LinuxImage
		}
		return defaultImage
	case "windows":
		return p.cfg.WindowsImage
	}
	return ""
}
func (p *Provider) DefaultJobImage() string { return "" }

func (p *Provider) Start(ctx context.Context, cfg providers.PollConfig) (<-chan providers.JobEvent, error) {
	ctx, p.cancel = context.WithCancel(ctx)

	// GitLab integration uses the custom executor model:
	// gitlab-runner handles registration and job polling.
	// ephemerd receives prepare/run/cleanup calls from gitlab-runner.
	//
	// Start() sets up the custom executor listener (a local HTTP or unix
	// socket server) that gitlab-runner's custom executor scripts call.
	if err := p.register(ctx); err != nil { //nolint:staticcheck // SA4023: register is a stub
		return nil, fmt.Errorf("gitlab runner registration: %w", err)
	}

	p.cfg.Log.Info("gitlab runner registered",
		"instance", p.cfg.InstanceURL,
		"runner_id", p.runnerID,
		"tags", p.cfg.Tags,
	)

	// Start the custom executor script server.
	// When gitlab-runner receives a job, it calls:
	//   1. prepare: ephemerd creates a container, returns build dir
	//   2. run: ephemerd execs the job script inside the container
	//   3. cleanup: ephemerd destroys the container
	// Each prepare call emits a JobEvent to the events channel.
	go p.listenForJobs(ctx)

	return p.events, nil
}

//nolint:staticcheck // SA4023: stub always errors — will return nil once registration is implemented
func (p *Provider) register(_ context.Context) error {
	// TODO: implement runner registration
	//
	// GitLab 16+ (runner authentication tokens):
	//   The token IS the authentication — no registration API call needed.
	//   POST /api/v4/runners/verify with the token to validate it.
	//
	// GitLab <16 (registration tokens):
	//   POST /api/v4/runners
	//   Body: { "token": p.cfg.Token, "tag_list": p.cfg.Tags }
	//   Response: { "id": 123, "token": "runner-auth-token" }
	return fmt.Errorf("gitlab: runner registration not yet implemented")
}

func (p *Provider) listenForJobs(ctx context.Context) {
	// TODO: implement custom executor server
	//
	// The custom executor model works via external scripts that
	// gitlab-runner calls at each stage. ephemerd provides these scripts
	// (or a single binary with subcommands):
	//
	//   ephemerd gitlab-exec prepare  — create container, print build dir
	//   ephemerd gitlab-exec run      — exec script inside container
	//   ephemerd gitlab-exec cleanup  — destroy container
	//
	// Alternatively, ephemerd can expose a local API that the scripts call,
	// keeping the scripts minimal (curl wrappers).
	//
	// When prepare is called, emit a JobEvent to p.events so the scheduler
	// can track the running job.
}

func (p *Provider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	// GitLab custom executor: gitlab-runner handles job claiming.
	// ephemerd just needs to create the container environment.
	// The runner token and instance URL are injected for any tools
	// inside the container that need to talk to GitLab.
	return &providers.Claim{
		RunnerID:   p.runnerID,
		RunnerName: runnerName,
		Repo:       event.Repo,
		Env: map[string]string{
			"CI_SERVER_URL":   p.cfg.InstanceURL,
			"CI_RUNNER_TOKEN": p.runnerToken,
		},
	}, nil
}

func (p *Provider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	// gitlab-runner handles job completion reporting.
	// Container cleanup is triggered by the cleanup script call.
	return nil
}

func (p *Provider) FetchJobImage(ctx context.Context, event *providers.JobEvent) string {
	// GitLab provides the image directly in the job payload —
	// the image: field from .gitlab-ci.yml is part of the job data.
	// No extra API call needed (unlike GitHub).
	//
	// TODO: extract from event.Raw when the job payload is available.
	return ""
}

func (p *Provider) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	// TODO: unregister runner from GitLab
	// DELETE /api/v4/runners/{runner_id}
	return nil
}
