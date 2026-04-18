---
title: "^# "
---


> **Status: Interface defined, GitHub adapter complete. Forgejo, Gitea, GitLab, and Woodpecker providers exist with e2e tests. Scheduler migration to Provider interface is pending (still uses `*github.Client` directly).**

## Overview

ephemerd uses a **Provider** interface (`pkg/providers/provider.go`) to abstract Git forge CI APIs. The scheduler works with any Provider without knowing which forge is behind it.

This allows ephemerd to support:

| Provider | Status | Runner Binary | Runner Image | Job Image | Job Discovery |
|----------|--------|---------------|--------------|-----------|---------------|
| **GitHub** | Working | `actions/runner` | `ghcr.io/actions/actions-runner:latest` | same container | Poll or webhook |
| **Forgejo** | E2E tested | `forgejo-runner` | `data.forgejo.org/forgejo/runner:12` | `gitea/runner-images:ubuntu-24.04` | Poll (ConnectRPC FetchTask) |
| **Gitea** | E2E tested | `act_runner` | `docker.io/gitea/act_runner:latest` | `gitea/runner-images:ubuntu-24.04` | Poll (ConnectRPC FetchTask) |
| **GitLab** | E2E tested | `gitlab-runner` | `ghcr.io/ephpm/runner-gitlab:latest` | managed by gitlab-runner | gitlab-runner custom executor |
| **Woodpecker** | E2E tested | `woodpecker-agent` | — | managed by agent | Woodpecker agent gRPC |

**Two-image model (Forgejo/Gitea):** The runner daemon runs in one container and creates job execution containers via the Docker API. ephemerd's fake Docker socket (`pkg/dind`) intercepts these calls. The `job_image` config controls the default execution environment.

## Architecture

```
┌──────────────────────────────────────────────────┐
│                  Scheduler                        │
│  (concurrency, dedup, routing, container lifecycle) │
│                                                    │
│  Works with providers.Provider — forge-agnostic   │
│  (NOTE: migration pending — currently uses        │
│   *github.Client directly)                        │
└──────────────────┬───────────────────────────────┘
                   │
         ┌─────────┼──────────┬─────────┬──────────┐
         │         │          │         │          │
         ▼         ▼          ▼         ▼          ▼
┌─────────────┐ ┌────────┐ ┌───────┐ ┌────────┐ ┌───────────┐
│   GitHub    │ │Forgejo │ │ Gitea │ │ GitLab │ │Woodpecker │
│  Provider   │ │Provider│ │Provider│ │Provider│ │ Provider  │
└──────┬──────┘ └───┬────┘ └───┬───┘ └───┬────┘ └─────┬─────┘
       │            │          │         │            │
       ▼            ▼          ▼         ▼            ▼
   GitHub API  ConnectRPC  ConnectRPC  gitlab-runner  Woodpecker
               (forgejo-   (act_runner  custom        agent gRPC
                runner)     binary)     executor
```

## Interfaces

There are three interfaces, split by capability:

```go
// Provider is the base — all platforms implement this.
type Provider interface {
    Name() string
    DefaultImage() string       // runner container image
    DefaultJobImage() string    // job execution image ("" if same container)
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

### Forgejo

Forgejo Actions uses GitHub Actions workflow syntax but a different runner: `forgejo-runner`, a hard fork of Gitea's `act_runner`. It embeds a fork of nektos/act and talks to the Forgejo instance over ConnectRPC.

ephemerd polls for tasks via the ConnectRPC FetchTask endpoint. When a job arrives, it spins up a container from the default runner image (`ghcr.io/ephpm/runner-forgejo:latest`) and launches:

```
forgejo-runner one-job \
    --url <instance_url> \
    --token-url file:///run/secrets/token \
    --label <labels> \
    --handle <task-uuid>
```

The `one-job --handle` command was designed for autoscalers: the runner claims the specific task, executes it, streams logs via UpdateLog, reports completion via UpdateTask, and exits.

- **Discovery**: ephemerd polls via ConnectRPC `FetchTask` (5 RPC service: Register, Declare, FetchTask, UpdateTask, UpdateLog)
- **ClaimJob**: No per-job registration. Injects `FORGEJO_INSTANCE_URL`, `FORGEJO_RUNNER_TOKEN`, `FORGEJO_RUNNER_UUID` into the container
- **ReleaseJob**: No-op — forgejo-runner handles UpdateTask
- **FetchJobImage**: Parse `workflow_payload` from task proto for `EPHEMERD_IMAGE` env var
- **Runner binary**: `forgejo-runner` (pre-installed in default image)
- **Proto package**: `code.forgejo.org/forgejo/actions-proto`
- **Key feature**: `--handle <uuid>` binds the runner to a specific task, preventing race conditions

See [forgejo-gitea.md](forgejo-gitea.md) for the full architecture, including the fake Docker socket deep-dive and Windows/macOS roadmap.

### Gitea

Gitea Actions shares the same workflow syntax and ConnectRPC protocol as Forgejo (both descend from the same codebase), but uses `act_runner` with different proto packages and a different ephemeral mode.

ephemerd polls for tasks via ConnectRPC FetchTask. When a job arrives, it spins up a container from the default runner image (`ghcr.io/ephpm/runner-gitea:latest`) and launches `act_runner daemon --ephemeral`, which runs one job and exits.

- **Discovery**: ephemerd polls via ConnectRPC `FetchTask` (same 5 RPC service as Forgejo)
- **ClaimJob**: No per-job registration. Injects `GITEA_INSTANCE_URL` and `GITEA_RUNNER_TOKEN` into the container
- **ReleaseJob**: No-op — act_runner handles UpdateTask
- **FetchJobImage**: Parse `workflow_payload` from task proto for `EPHEMERD_IMAGE` env var
- **Runner binary**: `act_runner` (pre-installed in default image)
- **Proto package**: `code.gitea.io/actions-proto-go`
- **Key difference from Forgejo**: No `--handle` flag — `--ephemeral` mode picks up the next available task rather than binding to a specific one. Single-task FetchTask (no batch support).

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

### Woodpecker CI

Woodpecker CI uses a server/agent architecture where agents connect to the server via gRPC.

- **Discovery**: The Woodpecker agent connects to the server and polls for jobs via gRPC
- **ClaimJob**: Agent registration uses a shared secret (`agent_secret`)
- **ReleaseJob**: Agent reports completion to server
- **Runner binary**: Woodpecker agent binary
- **Key difference**: Woodpecker requires a forge backend (Gitea/Forgejo/GitHub/GitLab) for repo management; ephemerd manages the agent lifecycle, not the server

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
# job_image = "gitea/runner-images:ubuntu-24.04"  # default job execution image

# === Gitea ===
[gitea]
instance_url = "https://gitea.example.com"
token = "runner-registration-token"
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional, omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"  # default job execution image

# === GitLab ===
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"
tags = ["linux", "docker", "ephemerd"]

# === Woodpecker CI ===
[woodpecker]
server_url = "woodpecker.example.com:9000"
agent_secret = "your-shared-secret"
```

Only one provider should be configured at a time. If multiple sections have credentials, the precedence is: Forgejo > Gitea > GitLab > Woodpecker > GitHub (GitHub is the default when nothing else is set).

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
        forgejo.go           # Forgejo Actions via forgejo-runner (stub)
    gitea/
        gitea.go             # Gitea Actions via act_runner (stub)
    gitlab/
        gitlab.go            # GitLab CI custom executor
    woodpecker/
        woodpecker.go        # Woodpecker CI agent
```

## Migration Path (Pending)

The scheduler currently takes `*github.Client` directly — this migration has not yet been done:

```go
// Current (not yet migrated):
type Config struct {
    GitHub *github.Client
    // ...
}

// Target:
type Config struct {
    Provider providers.Provider
    // ...
}
```

All `s.cfg.GitHub.*` calls will become `s.cfg.Provider.*` calls. The `github.JobEvent` type will be replaced by `providers.JobEvent` throughout the scheduler.

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
