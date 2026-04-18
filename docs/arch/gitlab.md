# Multi-Platform CI: JobProvider Interface for GitLab (and Beyond)

> **Status: Superseded by [providers.md](providers.md).** The Provider interface and stub implementations now exist in `pkg/providers/`. This document is preserved as historical context for the design decisions.

## Context

ephemerd currently only supports GitHub Actions. The scheduler, poller, webhook handler, and runner lifecycle are all hardwired to GitHub types and APIs. Adding GitLab CI support (or Gitea/Forgejo) requires decoupling the scheduler from GitHub.

This document explains why an interface abstraction is needed and what it looks like.

## Current Coupling

The scheduler (`pkg/scheduler/scheduler.go`) calls into `*github.Client` in five places:

1. **`s.cfg.GitHub.PollJobs(ctx)`** — lists queued workflow runs via GitHub REST API
2. **`s.cfg.GitHub.RegisterJITRunner(ctx, repo, name, labels)`** — creates a just-in-time runner via GitHub API
3. **`s.cfg.GitHub.RemoveRunner(ctx, repo, runnerID)`** — deregisters ghost runners
4. **`s.cfg.GitHub.FetchJobImage(ctx, repo, runID, jobID)`** — fetches workflow YAML from GitHub to read `EPHEMERD_IMAGE`
5. **`github.JobEvent`** — wraps `*gh.WorkflowJob`, a go-github struct with GitHub-specific methods (`GetRunID()`, `GetConclusion()`, `Labels`, etc.)

`JobEvent` flows through the entire scheduler: `pollLoop` -> `events` channel -> `handleQueued` -> `handleLocalJob`/`handleLinuxJob`. Every handler unpacks GitHub-specific fields.

The webhook handler in `pkg/github/client.go` parses `X-GitHub-Event: workflow_job` payloads and verifies `X-Hub-Signature-256` HMAC signatures — both GitHub-specific.

## How GitLab Differs

### Runner model

GitHub Actions uses **just-in-time (JIT) runners**: ephemerd registers a runner, gets a one-time config token, and passes it to the runner binary. The runner picks up exactly one job and deregisters.

GitLab uses **persistent runners**: a `gitlab-runner` process registers once, polls for jobs, and handles multiple jobs over its lifetime. GitLab also supports a **custom executor** model where `gitlab-runner` calls external prepare/run/cleanup scripts — this is the likely integration path for ephemerd.

There is no GitLab equivalent of `GenerateJITConfig`. Instead, ephemerd would either:
- Run `gitlab-runner` in custom executor mode with ephemerd-provided scripts, or
- Register a runner with the GitLab API and manage the `gitlab-runner` process lifecycle

### Job discovery

GitHub polling: `GET /repos/{owner}/{repo}/actions/runs?status=queued` -> list jobs per run -> filter by `self-hosted` label.

GitLab polling: not directly applicable. `gitlab-runner` polls GitLab for jobs itself (`POST /api/v4/jobs/request`). If ephemerd wraps `gitlab-runner`, the polling is handled by the runner binary, not by ephemerd. Alternatively, ephemerd could use the GitLab Jobs API (`GET /api/v4/projects/{id}/jobs?scope=pending`) but this doesn't provide the runner token handoff.

### Webhooks

GitHub: `X-GitHub-Event: workflow_job` with `action: "queued"/"completed"`, HMAC-SHA256 signature in `X-Hub-Signature-256`.

GitLab: `X-Gitlab-Event: Job Hook` with `build_status: "pending"/"success"/"failed"`, token-based verification via `X-Gitlab-Token` header (shared secret, no HMAC).

### Job image specification

GitHub: ephemerd reads `EPHEMERD_IMAGE` env var from the workflow YAML fetched via the Contents API.

GitLab: `.gitlab-ci.yml` has a first-class `image:` field per job. The image is part of the job payload from the API — no extra file fetch needed.

### Labels vs tags

GitHub: `runs-on: [self-hosted, linux, x64]` — labels on the workflow job.

GitLab: `tags: [linux, docker]` — tags on the job matched against runner tags at registration.

## Why an Interface

You cannot swap a different URL into `PollJobs()` and call it done. The differences are structural:

- **Runner lifecycle is inverted.** GitHub: register runner, get token, start runner, runner picks up job. GitLab custom executor: `gitlab-runner` picks up job, calls ephemerd scripts to prepare/run/cleanup environment.
- **Job discovery ownership differs.** GitHub: ephemerd polls. GitLab custom executor: `gitlab-runner` polls. ephemerd just responds to prepare/run/cleanup calls.
- **The event type is different.** `*gh.WorkflowJob` has fields that don't exist in GitLab (`RunID`, `Labels` as array, `Conclusion`). GitLab jobs have `pipeline_id`, `tag_list`, `status`.
- **Image resolution is different.** GitHub requires fetching and parsing workflow YAML. GitLab provides the image in the job payload.
- **Authentication is different.** GitHub: PAT or App installation token. GitLab: runner registration token or personal/project access token.
- **Webhook verification is different.** HMAC-SHA256 vs shared token header.

## Proposed Interface

```go
// JobProvider abstracts job discovery and runner lifecycle across CI platforms.
type JobProvider interface {
    // Poll returns jobs waiting for a runner.
    Poll(ctx context.Context) ([]Job, error)

    // PrepareRunner sets up a runner identity for the given job.
    // For GitHub: registers a JIT runner, returns encoded config.
    // For GitLab custom executor: may be a no-op (gitlab-runner handles registration).
    PrepareRunner(ctx context.Context, job Job, name string, labels []string) (RunnerRegistration, error)

    // RemoveRunner deregisters a runner (ghost cleanup on failure).
    RemoveRunner(ctx context.Context, job Job, runnerID int64) error

    // JobImage returns the container image for a job.
    // For GitHub: fetches workflow YAML and reads EPHEMERD_IMAGE env.
    // For GitLab: reads the image field from the job payload.
    JobImage(ctx context.Context, job Job) string

    // WebhookHandler returns an HTTP handler for push-based job events.
    // Returns nil if the provider doesn't support webhooks.
    WebhookHandler(secret string) (http.Handler, <-chan Job)
}
```

### Platform-neutral types

```go
// Job is the common job representation used by the scheduler.
type Job struct {
    ID         int64
    ExternalID string   // platform-specific ID for API calls
    Repo       string   // "repo" for GitHub, "project" for GitLab
    Action     string   // "queued", "completed", "pending", "success", etc.
    Labels     []string // GitHub labels or GitLab tags
    Image      string   // container image, if known from the payload
    Platform   string   // "github", "gitlab", etc.
    Raw        any      // original platform-specific object for edge cases
}

// RunnerRegistration holds the result of PrepareRunner.
type RunnerRegistration struct {
    RunnerID    int64
    EncodedConfig string // JIT config for GitHub, empty for GitLab custom executor
}
```

## GitLab Integration Path

The most natural GitLab integration is the **custom executor** model:

1. ephemerd installs and manages a `gitlab-runner` process configured with custom executor scripts
2. `gitlab-runner` polls GitLab for jobs (ephemerd doesn't poll)
3. When a job arrives, `gitlab-runner` calls ephemerd's prepare script -> ephemerd creates a container
4. `gitlab-runner` calls run script -> executes job steps inside the container
5. `gitlab-runner` calls cleanup script -> ephemerd destroys the container

This means `Poll()` would not be used for GitLab — job discovery is handled by `gitlab-runner`. The `JobProvider` interface still works: `Poll()` returns empty, and the custom executor scripts create `Job` events that feed into the same scheduler event loop.

Alternatively, ephemerd could implement the runner protocol directly (skipping `gitlab-runner`) but that's significantly more work and the custom executor model is well-documented and stable.

## Scheduler Changes

The scheduler currently takes `*github.Client` directly:

```go
type Config struct {
    GitHub *github.Client
    // ...
}
```

This becomes:

```go
type Config struct {
    Provider JobProvider
    // ...
}
```

All direct `s.cfg.GitHub.*` calls become `s.cfg.Provider.*` calls. The `github.JobEvent` type is replaced by the platform-neutral `Job` type throughout the scheduler, dispatch layer, and running job tracking.

The webhook handler setup in `Scheduler.Run()` calls `s.cfg.Provider.WebhookHandler()` and only mounts the handler if non-nil.

## What Stays the Same

- Container runtime (`pkg/runtime`) — platform-agnostic, works with any CI system
- WSL dispatch architecture — routing Linux/Windows jobs is orthogonal to CI provider
- Networking, containerd lifecycle, runner binary extraction — all unchanged
- gRPC control plane (status, jobs, drain) — unchanged
- Concurrency limiting, dedup, drain logic — unchanged (dedup keys become `Job.ID`)
