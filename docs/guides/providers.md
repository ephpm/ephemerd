---
title: Providers
weight: 2
---

ephemerd supports five CI providers. The provider determines how jobs are discovered, how runners are registered, and which runner binary executes inside the container. Everything else -- container runtime, networking, VM support, concurrency control -- is shared across all providers.

## Auto-Detection

Currently only one provider can be configured. The provider is auto-detected from which configuration section has credentials set. The precedence order is:

**Forgejo > Gitea > GitLab > Woodpecker > GitHub**

GitHub is the default when no other provider section is configured.

## GitHub

The primary provider. ephemerd polls the GitHub API (or receives webhooks) for queued workflow jobs, registers a JIT (just-in-time) runner for each one, and deregisters it after the job completes.

**Authentication:** Personal Access Token or GitHub App.

**Job discovery:** Polling (default) or webhooks (via tunnel or direct TLS). See [Job Discovery]({{< relref "job-discovery" >}}) for details.

**Runner binary:** The official GitHub Actions runner (`actions/runner`), embedded in the ephemerd binary.

```toml
[github]
owner = "your-org"
repos = ["repo1", "repo2"]   # optional: omit for org-level runners
# poll_interval = "10s"

# PAT auth (or set GITHUB_TOKEN env var):
# token = "ghp_xxx"

# GitHub App auth:
# app_id = 123456
# installation_id = 789012
# private_key_path = "/var/lib/ephemerd/app.pem"
```

## Forgejo

Forgejo Actions uses GitHub Actions workflow syntax but a different runner: `forgejo-runner`, a fork of Gitea's `act_runner`. ephemerd polls for tasks via the ConnectRPC `FetchTask` endpoint.

When a job arrives, ephemerd creates a container from the runner image and launches `forgejo-runner one-job --handle <task-uuid>`. The `--handle` flag binds the runner to a specific task, preventing race conditions when multiple runners are active.

Forgejo uses a two-container model: the runner daemon runs in one container and creates job execution containers via the Docker API. ephemerd's fake Docker socket intercepts these calls and translates them into containerd operations, so no actual Docker daemon is needed.

**Authentication:** Runner registration token from Forgejo admin.

**Job discovery:** ConnectRPC `FetchTask` polling.

**Runner binary:** `forgejo-runner` (pre-installed in the runner image).

```toml
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional: omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"
```

## Gitea

Gitea Actions shares the same workflow syntax and ConnectRPC protocol as Forgejo (both descend from the same codebase), but uses `act_runner` with a different ephemeral mode. ephemerd polls for tasks via ConnectRPC `FetchTask` and launches `act_runner daemon --ephemeral`, which runs one job and exits.

The key difference from Forgejo is the lack of a `--handle` flag. The `--ephemeral` mode picks up the next available task rather than binding to a specific one. Gitea also uses a different protobuf package (`code.gitea.io/actions-proto-go` vs Forgejo's `code.forgejo.org/forgejo/actions-proto`).

Like Forgejo, Gitea uses the two-container model with ephemerd's fake Docker socket.

**Authentication:** Runner registration token from Gitea admin.

**Job discovery:** ConnectRPC `FetchTask` polling.

**Runner binary:** `act_runner` (pre-installed in the runner image).

```toml
[gitea]
instance_url = "https://gitea.example.com"
token = "runner-registration-token"
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional: omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"
```

## GitLab

GitLab CI uses a custom executor model where `gitlab-runner` drives the job lifecycle. Unlike the other providers, ephemerd does not discover jobs directly. Instead, `gitlab-runner` polls GitLab for jobs and calls ephemerd scripts to prepare, run, and clean up containers.

The lifecycle is inverted: gitlab-runner receives the job and calls ephemerd to manage the container. ephemerd responds to requests rather than initiating them.

```
gitlab-runner                    ephemerd
     |                              |
     |<-- polls GitLab for jobs --> |
     |     (gitlab-runner drives)   |
     |                              |
     |-- prepare -----------------> | create container
     |<-- build_dir path ---------- |
     |                              |
     |-- run (job script) --------> | exec in container
     |<-- exit code --------------- |
     |                              |
     |-- cleanup -----------------> | destroy container
     |                              |
```

**Authentication:** Runner authentication token (`glrt-xxx` for GitLab 16+).

**Job discovery:** `gitlab-runner` polls GitLab. ephemerd does not poll GitLab directly.

**Runner binary:** Official `gitlab-runner` in custom executor mode.

```toml
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"
tags = ["linux", "docker", "ephemerd"]
```

## Woodpecker CI

Woodpecker CI uses a server/agent architecture. The Woodpecker agent connects to the Woodpecker server via gRPC and polls for jobs using a shared secret for authentication. ephemerd manages the agent lifecycle and container execution.

Woodpecker requires a forge backend (Gitea, Forgejo, GitHub, or GitLab) for repository management. ephemerd handles the agent side only -- it does not replace the Woodpecker server or the forge backend.

**Authentication:** Shared secret between agent and server.

**Job discovery:** Woodpecker agent gRPC connection to server.

**Runner binary:** Woodpecker agent binary.

```toml
[woodpecker]
server_url = "woodpecker.example.com:9000"
agent_secret = "your-shared-secret"
```

## What Stays the Same

Regardless of which provider is active, the following subsystems are shared:

- Container runtime (containerd, OCI images, overlayfs/windows snapshotter)
- WSL2 dispatch for Linux jobs on Windows hosts
- macOS VM support via Virtualization.framework
- CNI bridge networking (Linux) and HCN NAT networking (Windows)
- Concurrency limiting, job dedup, and graceful drain
- Fake Docker socket for Forgejo/Gitea two-container model
- gRPC control plane (`ephemerd status`, `ephemerd jobs`)
