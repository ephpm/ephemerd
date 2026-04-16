# Providers: Multi-Forge CI Integration

> **Status: Interface defined, GitHub adapter complete, Forgejo and GitLab are stubs.**

## Overview

ephemerd uses a **Provider** interface (`pkg/providers/provider.go`) to abstract Git forge CI APIs. The scheduler works with any Provider without knowing which forge is behind it.

This allows ephemerd to support:

| Provider | Status | Runner Model | Job Discovery |
|----------|--------|--------------|---------------|
| **GitHub** | Working | JIT runner per job | Poll or webhook |
| **Forgejo** | Stub | Persistent runner + FetchTask | Poll |
| **GitLab** | Stub | Custom executor scripts | gitlab-runner polls |

## Architecture

```
┌──────────────────────────────────────────────────┐
│                  Scheduler                        │
│  (concurrency, dedup, routing, container lifecycle) │
│                                                    │
│  Works with providers.Provider — forge-agnostic   │
└──────────────────┬───────────────────────────────┘
                   │
         ┌─────────┼─────────┐
         │         │         │
         ▼         ▼         ▼
┌─────────────┐ ┌────────┐ ┌────────┐
│   GitHub    │ │Forgejo │ │ GitLab │
│  Provider   │ │Provider│ │Provider│
└──────┬──────┘ └───┬────┘ └───┬────┘
       │            │          │
       ▼            ▼          ▼
   GitHub API   Forgejo API  gitlab-runner
                             custom executor
```

## Interfaces

There are three interfaces, split by capability:

```go
// Provider is the base — all platforms implement this.
type Provider interface {
    Name() string
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
| GitHub   | Yes | Yes |
| Forgejo  | Yes | No  |
| GitLab   | Yes | No  |

The scheduler type-asserts for `Webhook` when tunnel/TLS is configured:

```go
if wp, ok := provider.(providers.Webhook); ok {
    handler, whEvents := wp.WebhookHandler(secret)
    mux.Handle("/webhook", handler)
}
```

### Job Lifecycle

1. **Start()** — provider begins polling for jobs, returns event channel
2. **ClaimJob()** — scheduler accepts a queued job, provider registers a runner
3. **FetchJobImage()** — provider looks up custom container image
4. **ReleaseJob()** — job done, provider deregisters the runner
5. **Stop()** — shutdown, clean up connections

## How Each Provider Works

### GitHub

The existing integration, adapted into the Provider interface.

- **Discovery**: Poll `GET /repos/.../actions/runs?status=queued` or receive `workflow_job` webhooks
- **ClaimJob**: Register a JIT runner via `POST /repos/.../actions/runners/registrations/jit`, returns base64-encoded config passed to the runner binary as `--jitconfig`
- **ReleaseJob**: `DELETE /repos/.../actions/runners/{id}`
- **FetchJobImage**: Fetch workflow YAML from GitHub Contents API, parse `EPHEMERD_IMAGE` env var
- **Runner binary**: Official GitHub Actions runner (`actions/runner`), embedded by ephemerd

### Forgejo (and Gitea)

Forgejo Actions uses GitHub Actions workflow syntax but a different runner protocol. Gitea uses the same protocol (Forgejo forked from Gitea; both use `runner.v1.RunnerService`).

Rather than reimplement the protocol or embed a new runner binary, ephemerd runs the official `forgejo-runner` inside an ephemerd-managed container with the fake Docker socket (`pkg/dind`) mounted. The runner polls Forgejo, act (inside the runner) creates job containers via the fake socket, and ephemerd destroys everything when done.

See [forgejo.md](forgejo.md) for the full architecture, including the two-level container model and mermaid diagrams.

- **Discovery**: `forgejo-runner` (inside the container) polls Forgejo via FetchTask. Ephemerd maintains a pool of N ephemeral runner containers.
- **ClaimJob**: Returns the `forgejo-runner` image, registration token, and fake Docker socket mount. No protocol client in ephemerd.
- **ReleaseJob**: Destroys the runner container and all sibling containers it spawned (job container + services). No API call to Forgejo needed — forgejo-runner reports completion via UpdateTask.
- **FetchJobImage**: N/A — act handles image selection based on `runs-on:` labels.
- **Runner binary**: `code.forgejo.org/forgejo/runner:6` (image-based, not embedded). Same approach works for Gitea by swapping to `gitea/act_runner`.
- **Key advantage**: Zero reimplementation. Everything works via the existing fake Docker socket that already exists for `docker run` support in jobs.

### GitLab

GitLab CI uses a custom executor model where `gitlab-runner` drives the job lifecycle.

- **Discovery**: `gitlab-runner` polls GitLab for jobs — ephemerd does NOT poll GitLab directly
- **ClaimJob**: No per-job registration (gitlab-runner handles this). Injects `CI_SERVER_URL` and `CI_RUNNER_TOKEN` into the container
- **ReleaseJob**: No-op (gitlab-runner reports completion)
- **FetchJobImage**: The `image:` field from `.gitlab-ci.yml` is part of the job payload — no extra API call needed
- **Runner binary**: Official `gitlab-runner` in custom executor mode
- **Key difference**: The lifecycle is inverted. gitlab-runner receives the job and calls ephemerd scripts to prepare/run/cleanup the container. ephemerd doesn't discover jobs — it responds to requests

**Custom executor flow:**

```
gitlab-runner                    ephemerd
     │                              │
     │◄── poll GitLab for jobs ────►│
     │     (gitlab-runner does this) │
     │                              │
     ├── prepare ──────────────────►│ create container
     │◄── build_dir path ──────────┤
     │                              │
     ├── run (job script) ─────────►│ exec in container
     │◄── exit code ───────────────┤
     │                              │
     ├── cleanup ──────────────────►│ destroy container
     │                              │
```

## Configuration

Provider is auto-detected from which section has credentials:

```toml
# === GitHub (default) ===
[github]
owner = "your-org"
# token via GITHUB_TOKEN env, or:
# app_id = 123456
# installation_id = 789012
# private_key_path = "/path/to/app.pem"

# === Forgejo ===
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional, omit for all repos

# === GitLab ===
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"
tags = ["linux", "docker", "ephemerd"]
```

Only one provider should be configured at a time. If multiple sections have credentials, the precedence is: Forgejo > GitLab > GitHub (GitHub is the default when nothing else is set).

## What Stays the Same Across Providers

- Container runtime (`pkg/runtime`) — provider-agnostic
- WSL dispatch (Linux jobs on Windows) — orthogonal to CI provider
- Networking, containerd, runner binary extraction — unchanged
- gRPC control plane (status, jobs, drain) — unchanged
- Concurrency limiting, dedup, graceful drain — unchanged
- Docker-in-Docker fake daemon — unchanged
- macOS VM support — unchanged

## Package Layout

```
pkg/providers/
    provider.go              # Provider interface + shared types
    github/
        github.go            # wraps existing pkg/github.Client
    forgejo/
        forgejo.go           # Forgejo Actions integration (stub)
    gitlab/
        gitlab.go            # GitLab CI custom executor (stub)
```

## Migration Path

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
    Provider providers.Provider
    // ...
}
```

All `s.cfg.GitHub.*` calls become `s.cfg.Provider.*` calls. The `github.JobEvent` type is replaced by `providers.JobEvent` throughout the scheduler.

This is a refactor of the scheduler internals only — no changes to container runtime, networking, VM support, or the CLI.

## E2E Testing

Each provider has its own e2e test strategy. The goal is a fully self-contained test that boots the platform, provisions test data, runs a real workflow, and tears down — no external accounts or infrastructure needed.

### Forgejo (implemented)

Forgejo is lightweight enough to run as part of the e2e suite. The test (`test/e2e/forgejo/forgejo_test.go`) does everything in-process:

1. Detects `docker compose` (v2 plugin or standalone binary)
2. Writes a compose file to a temp dir and boots Forgejo (`codeberg.org/forgejo/forgejo:9`)
3. Waits for health via `GET /api/v1/version` (typically 3-8 seconds)
4. Creates an admin user via `forgejo admin user create` inside the container
5. Obtains an API token via `POST /api/v1/users/admin/tokens`
6. Creates a test org and repo via API
7. Pushes a workflow file to `.forgejo/workflows/test.yaml` (the push triggers a run)
8. Gets a runner registration token via `GET /api/v1/repos/.../actions/runners/registration-token`
9. Starts a `forgejo-runner` container on the same Docker network, registered against the Forgejo instance
10. Polls `GET /api/v1/repos/.../actions/runs` until the workflow run completes
11. Asserts the run status is `success`
12. `t.Cleanup` and `defer` tear down all containers regardless of pass/fail

Run with:

```bash
mage e2eforgejo
```

Requires Docker with compose support. No root/sudo needed — just Docker access.

### GitLab (planned)

GitLab CE (`gitlab/gitlab-ce`) can also run as a container, but it needs 4+ GB RAM and takes 2-3 minutes to boot. The test would follow the same pattern (API-driven setup, custom executor registration) but is better suited as an optional/manual test rather than part of the fast e2e suite.

### GitHub (existing)

GitHub e2e tests use the real GitHub API with a test org and PAT. These run as part of `mage e2e` and require `GITHUB_TOKEN` to be set.
