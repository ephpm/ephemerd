// Package providers defines the Provider interface for CI job discovery
// and runner lifecycle management.
//
// Each supported platform implements Provider
// so the scheduler can discover jobs, register runners, and clean up
// without knowing which platform it's talking to.
//
// Job lifecycle from the scheduler's perspective:
//
//  1. Start()         — begin discovering jobs (polling, webhooks, or task streams)
//  2. ClaimJob()      — accept a queued job, register a runner, get container config
//  3. FetchJobImage() — look up a custom container image for the job
//  4. ReleaseJob()    — deregister runner, clean up after completion/failure
//  5. Stop()          — shutdown, clean up webhooks/connections
package providers

import (
	"context"
	"net/http"
)

// Provider is the base interface that all platform integrations implement.
// It handles runner registration, job claiming, and cleanup.
type Provider interface {
	// Name returns the provider identifier (e.g., "github", "forgejo", "gitea", "gitlab").
	Name() string

	// DefaultImage returns the default OCI container image for this provider.
	// This is the image that contains the runner binary/daemon:
	//   - GitHub:  ghcr.io/actions/actions-runner:latest  (runner inside container)
	//   - Forgejo: data.forgejo.org/forgejo/runner:12     (runner daemon)
	//   - Gitea:   docker.io/gitea/act_runner:latest      (runner daemon)
	//   - GitLab:  ghcr.io/ephpm/runner-gitlab:latest     (gitlab-runner)
	//
	// For Forgejo/Gitea, this is the runner daemon container. The runner
	// creates separate job containers via the fake Docker socket (pkg/dind).
	// See DefaultJobImage() for the job execution environment.
	DefaultImage() string

	// DefaultJobImage returns the default OCI image for job execution.
	// This is the environment where workflow steps actually run.
	//   - GitHub:  "" (runner and job share the same container)
	//   - Forgejo: gitea/runner-images:ubuntu-24.04 (runner creates via Docker API)
	//   - Gitea:   gitea/runner-images:ubuntu-24.04 (runner creates via Docker API)
	//   - GitLab:  "" (gitlab-runner manages job containers)
	//
	// For GitHub, this returns "" because the runner executes steps directly
	// inside its own container. For Forgejo/Gitea, the runner daemon uses
	// the Docker API (intercepted by pkg/dind) to create a separate job
	// container from this image.
	DefaultJobImage() string

	// ClaimJob accepts a queued job and returns the configuration needed
	// to start a runner inside the container.
	//   - GitHub:  registers a per-job JIT runner, returns encoded --jitconfig
	//   - Forgejo: returns instance URL + token for forgejo-runner one-job
	//   - Gitea:   returns instance URL + token for act_runner --ephemeral
	//   - GitLab:  may be a no-op (gitlab-runner handles its own registration)
	ClaimJob(ctx context.Context, event *JobEvent, runnerName string, labels []string) (*Claim, error)

	// ReleaseJob cleans up after a job completes or fails.
	// Deregisters the runner, frees server-side resources, etc.
	ReleaseJob(ctx context.Context, claim *Claim) error

	// FetchJobImage returns a custom container image for the job, if specified
	// in the workflow/pipeline definition.
	//   - GitHub/Forgejo/Gitea: fetches workflow YAML and reads EPHEMERD_IMAGE env var
	//   - GitLab: reads the image: field from the job payload directly
	// Returns empty string if none.
	FetchJobImage(ctx context.Context, event *JobEvent) string

	// Stop performs shutdown cleanup (deregister webhooks, close connections).
	Stop(ctx context.Context) error
}

// Poll is implemented by providers that discover jobs by polling the platform API.
// All providers support polling. The scheduler calls Start to begin discovery
// and reads job events from the returned channel.
type Poll interface {
	Provider

	// Start begins polling for jobs and returns a channel of events.
	// The channel is closed when ctx is cancelled or Stop is called.
	Start(ctx context.Context, cfg PollConfig) (<-chan JobEvent, error)
}

// Webhook is optionally implemented by providers that support inbound
// webhook delivery for faster job discovery. The scheduler checks for
// this interface and mounts the handler on its HTTP server if available.
// Providers that don't support webhooks should not implement this interface.
type Webhook interface {
	Provider

	// WebhookHandler returns an HTTP handler for receiving platform webhooks
	// and a channel that emits job events parsed from webhook payloads.
	WebhookHandler(secret string) (http.Handler, <-chan JobEvent)

	// RegisterWebhooks creates webhooks on the platform pointing at the given URL.
	RegisterWebhooks(ctx context.Context, url, secret string) error

	// DeregisterWebhooks removes webhooks created by RegisterWebhooks.
	DeregisterWebhooks(ctx context.Context) error
}

// PollConfig provides settings for poll-based job discovery.
type PollConfig struct {
	PollInterval int // seconds between polls (0 = provider default)
}

// JobEvent represents a CI job state change from the forge.
type JobEvent struct {
	Action     string   // "queued" or "completed"
	Repo       string   // repository identifier (e.g., "myrepo" or "group/project")
	JobID      int64    // forge-specific job ID
	RunID      int64    // workflow run ID (GitHub/Forgejo) or pipeline ID (GitLab)
	Labels     []string // runner labels/tags (e.g., ["self-hosted", "linux", "x64"])
	Conclusion string   // for completed events: "success", "failure", "cancelled"

	// Raw holds the original forge-specific object for edge cases.
	// GitHub: *github.WorkflowJob, Forgejo: task proto, GitLab: job payload.
	Raw any
}

// Claim is returned by ClaimJob and tracks a runner registered for a job.
type Claim struct {
	RunnerID   int64
	RunnerName string
	Repo       string

	// RunnerConfig is an opaque config string passed to the runner binary.
	//   GitHub:  base64-encoded JIT config (passed via --jitconfig flag)
	//   Forgejo: unused (runner uses token + URL from Env)
	//   GitLab:  unused (gitlab-runner manages its own config)
	RunnerConfig string

	// Env holds extra environment variables injected into the runner container.
	// Used by Forgejo/GitLab to pass instance URL, runner token, etc.
	Env map[string]string
}
