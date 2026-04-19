---
title: Multi-Forge Providers
weight: 8
---

ephemerd uses a Provider interface to abstract Git forge CI APIs. The scheduler works with any provider without knowing which forge is behind it.

> **Status:** Interface defined, GitHub adapter complete. Forgejo, Gitea, GitLab, and Woodpecker providers exist with e2e tests. Scheduler migration to Provider interface is pending -- it still uses `*github.Client` directly.

## Supported Providers

| Provider | Status | Runner Binary | Job Discovery |
|----------|--------|---------------|---------------|
| **GitHub** | Working | `actions/runner` | Poll or webhook |
| **Forgejo** | E2E tested | `forgejo-runner` | Poll (ConnectRPC FetchTask) |
| **Gitea** | E2E tested | `act_runner` | Poll (ConnectRPC FetchTask) |
| **GitLab** | E2E tested | `gitlab-runner` | gitlab-runner custom executor |
| **Woodpecker** | E2E tested | `woodpecker-agent` | Woodpecker agent gRPC |

## Provider Interface

The interface is defined in `pkg/providers/provider.go` with three types split by capability:

```go
// Provider is the base -- all platforms implement this.
type Provider interface {
    Name() string
    DefaultImage() string
    DefaultJobImage() string
    ClaimJob(ctx, event, name, labels) (*Claim, error)
    ReleaseJob(ctx, claim) error
    FetchJobImage(ctx, event) string
    Stop(ctx) error
}

// Poll is implemented by all providers for job discovery via polling.
type Poll interface {
    Provider
    Start(ctx, cfg PollConfig) (<-chan JobEvent, error)
}

// Webhook is optionally implemented by providers that support
// inbound webhook delivery for faster job discovery.
type Webhook interface {
    Provider
    WebhookHandler(secret) (http.Handler, <-chan JobEvent)
    RegisterWebhooks(ctx, url, secret) error
    DeregisterWebhooks(ctx) error
}
```

| Provider | Implements Poll | Implements Webhook |
|----------|:-:|:-:|
| GitHub     | Yes | Yes |
| Forgejo    | Yes | No  |
| Gitea      | Yes | No  |
| GitLab     | Yes | No  |
| Woodpecker | Yes | No  |

The scheduler type-asserts for `Webhook` when a tunnel or TLS is configured:

```go
if wp, ok := provider.(providers.Webhook); ok {
    handler, whEvents := wp.WebhookHandler(secret)
    mux.Handle("/webhook", handler)
}
```

## Job Lifecycle

From the scheduler's perspective:

1. **Start()** -- provider begins polling for jobs, returns an event channel.
2. **ClaimJob()** -- scheduler accepts a queued job, provider registers a runner.
3. **FetchJobImage()** -- provider looks up a custom container image for the job.
4. **ReleaseJob()** -- job done, provider deregisters the runner.
5. **Stop()** -- shutdown, clean up connections.

## How Each Provider Works

### GitHub

- **Discovery**: poll `GET /repos/.../actions/runs?status=queued` or receive `workflow_job` webhooks.
- **ClaimJob**: register a JIT runner via `POST /repos/.../actions/runners/registrations/jit`, returns base64-encoded config passed via `--jitconfig`.
- **ReleaseJob**: `DELETE /repos/.../actions/runners/{id}`.
- **Runner binary**: official GitHub Actions runner (`actions/runner`), embedded by ephemerd.

### Forgejo

Forgejo Actions uses GitHub Actions workflow syntax but a different runner: `forgejo-runner`, a hard fork of Gitea's `act_runner`. It embeds a fork of nektos/act and talks to the Forgejo instance over ConnectRPC.

- **Discovery**: ephemerd polls via ConnectRPC `FetchTask`.
- **ClaimJob**: injects `FORGEJO_INSTANCE_URL`, `FORGEJO_RUNNER_TOKEN`, `FORGEJO_RUNNER_UUID` into the container.
- **Ephemeral mode**: `one-job --handle <uuid>` binds the runner to a specific task, preventing race conditions.
- **Proto package**: `code.forgejo.org/forgejo/actions-proto`.

### Gitea

Gitea Actions shares the same workflow syntax and ConnectRPC protocol as Forgejo, but uses `act_runner` with different proto packages.

- **Discovery**: ephemerd polls via ConnectRPC `FetchTask`.
- **ClaimJob**: injects `GITEA_INSTANCE_URL` and `GITEA_RUNNER_TOKEN` into the container.
- **Ephemeral mode**: `act_runner daemon --ephemeral` (no `--handle` flag -- picks up the next available task).
- **Proto package**: `code.gitea.io/actions-proto-go`.

### GitLab

GitLab CI uses a custom executor model where `gitlab-runner` drives the job lifecycle. The lifecycle is inverted -- gitlab-runner receives the job and calls ephemerd scripts to prepare/run/cleanup the container.

- **Discovery**: `gitlab-runner` polls GitLab for jobs -- ephemerd does not poll GitLab directly.
- **Custom executor flow**: `prepare` (create container) -> `run` (exec steps) -> `cleanup` (destroy container).

### Woodpecker

Woodpecker CI uses a server/agent architecture where agents connect to the server via gRPC.

- **Discovery**: the Woodpecker agent connects to the server and polls for jobs.
- **ClaimJob**: agent registration uses a shared secret (`agent_secret`).

## Configuration

Currently only one provider can be configured. The provider is auto-detected from which config section has credentials:

```toml
# GitHub (default when nothing else is set)
[github]
owner = "your-org"

# Forgejo
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"
owner = "your-org"

# Gitea
[gitea]
instance_url = "https://gitea.example.com"
token = "runner-registration-token"
owner = "your-org"

# GitLab
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"
tags = ["linux", "docker", "ephemerd"]

# Woodpecker CI
[woodpecker]
server_url = "woodpecker.example.com:9000"
agent_secret = "your-shared-secret"
```

Only one provider should be configured at a time. Precedence when multiple sections have credentials: Forgejo > Gitea > GitLab > Woodpecker > GitHub.

## What Stays the Same Across Providers

The entire container infrastructure is provider-agnostic:

- Container runtime (`pkg/runtime`)
- WSL dispatch (Linux jobs on Windows)
- Networking (CNI on Linux, HCN on Windows)
- Embedded containerd
- gRPC control plane (status, jobs, drain)
- Concurrency limiting, dedup, graceful drain
- Fake Docker daemon (`pkg/dind`)
- macOS VM support

## Package Layout

```
pkg/providers/
    provider.go              # Provider interface + shared types
    github/
        github.go            # wraps existing pkg/github.Client
    forgejo/
        forgejo.go           # Forgejo Actions via forgejo-runner
    gitea/
        gitea.go             # Gitea Actions via act_runner
    gitlab/
        gitlab.go            # GitLab CI custom executor
    woodpecker/
        woodpecker.go        # Woodpecker CI agent
```

## Migration Path (Pending)

The scheduler currently takes `*github.Client` directly. The planned migration:

```go
// Current (not yet migrated):
type Config struct {
    GitHub *github.Client
}

// Target:
type Config struct {
    Provider providers.Provider
}
```

All `s.cfg.GitHub.*` calls will become `s.cfg.Provider.*` calls. This is a refactor of scheduler internals only -- no changes to container runtime, networking, VM support, or the CLI.
