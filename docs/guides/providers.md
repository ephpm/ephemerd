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

Forgejo Actions uses GitHub Actions workflow syntax but a different protocol. There are two ways to run Forgejo jobs:

### Option 1: ephemerd-runner-forgejo (recommended)

ephemerd includes **ephemerd-runner-forgejo** — a lightweight Go binary that speaks the Forgejo ConnectRPC protocol and executes workflow steps directly via `os/exec`. Single container, no Docker socket needed. Supports Linux, Windows, and macOS.

ephemerd-runner-forgejo handles `run:` steps. `uses:` steps (actions) are not yet supported — if your workflows rely heavily on actions, use Option 2.

**Runner binary:** `ephemerd-runner-forgejo` (built into ephemerd).

### Option 2: upstream forgejo-runner + fake Docker socket

Use the upstream `forgejo-runner` with ephemerd's fake Docker socket (`pkg/dind`). This is the two-container model — the runner daemon runs in one container and creates a separate job container via the Docker API. ephemerd intercepts those Docker API calls and translates them to containerd operations.

This option has full `uses:` action support via the embedded nektos/act engine, but only works on Linux.

**Runner binary:** `forgejo-runner` (pre-installed in the runner image).

### Config

**Authentication:** Runner registration token from Forgejo admin.

**Job discovery:** ConnectRPC `FetchTask` polling.

```toml
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional: omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"
```

## Gitea

Gitea Actions shares the same workflow syntax and ConnectRPC protocol as Forgejo. The same two options apply — ephemerd-runner-forgejo (as `gitea-runner`) for the single-container model, or upstream `act_runner` with the fake Docker socket for full action support.

**Authentication:** Runner registration token from Gitea admin.

**Job discovery:** ConnectRPC `FetchTask` polling.

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
- Hyper-V Linux VM dispatch for Linux jobs on Windows hosts
- macOS VM support via Virtualization.framework
- CNI bridge networking (Linux) and HCN NAT networking (Windows)
- Concurrency limiting, job dedup, and graceful drain
- gRPC control plane (`ephemerd status`, `ephemerd jobs`)
