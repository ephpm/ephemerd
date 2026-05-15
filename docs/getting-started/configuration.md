---
title: Configuration
weight: 3
---

ephemerd is configured with a single TOML file at `<data-dir>/config.toml`:

- **Linux / macOS:** `/var/lib/ephemerd/config.toml`
- **Windows:** `C:\ProgramData\ephemerd\config.toml`

Override the data directory with `--data-dir`:

```bash
ephemerd serve --data-dir /opt/ephemerd
```

## Provider auto-detection

Currently only one provider can be configured. ephemerd detects the active provider based on which section has credentials set, in this order:

1. **Forgejo** -- if `forgejo.instance_url` is set
2. **Gitea** -- if `gitea.instance_url` is set
3. **GitLab** -- if `gitlab.instance_url` is set
4. **Woodpecker** -- if `woodpecker.server_url` is set
5. **GitHub** -- default when none of the above are configured

---

## Complete annotated example

```toml
# =============================================================================
# ephemerd configuration
# =============================================================================

# --- GitHub Actions (default provider) ----------------------------------------
[github]
owner = "your-org"
# repos = ["repo1", "repo2"]        # optional — omit for org-level runners

# Authentication: PAT or GitHub App (choose one)
# token = "ghp_..."                  # or set GITHUB_TOKEN env var
# app_id = 12345
# installation_id = 67890
# private_key_path = "/etc/ephemerd/app.pem"

# poll_interval = "30s"             # how often to poll for queued jobs

# --- Forgejo Actions ---------------------------------------------------------
# [forgejo]
# instance_url = "https://codeberg.org"
# token = "runner-registration-token"
# owner = "your-org"                 # optional — omit for instance-level runners
# repos = ["repo1"]                  # optional — omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"

# --- Gitea Actions -----------------------------------------------------------
# [gitea]
# instance_url = "https://gitea.example.com"
# token = "runner-registration-token"
# owner = "your-org"
# repos = ["repo1"]
# job_image = "gitea/runner-images:ubuntu-24.04"

# --- GitLab CI ----------------------------------------------------------------
# [gitlab]
# instance_url = "https://gitlab.com"
# token = "glrt-xxxxxxxxxxxxxxxxxxxx"  # runner auth token (GitLab 16+)
# tags = ["linux", "docker"]

# --- Woodpecker CI -----------------------------------------------------------
# [woodpecker]
# server_url = "woodpecker.example.com:9000"
# agent_secret = "shared-secret"

# --- Webhook delivery --------------------------------------------------------
[webhook]
# tunnel = "none"                    # "none" (polling), "localtunnel", or "ngrok"
# tunnel_url = ""                    # localtunnel: self-hosted server URL
# ngrok_authtoken = ""               # ngrok auth token (or NGROK_AUTHTOKEN env)
# secret = ""                        # HMAC secret (auto-generated if tunnel is active)
# port = 8080                        # listen port for webhook/health endpoint
# tls_cert = ""                      # TLS cert path (direct TLS, no tunnel)
# tls_key = ""                       # TLS key path

# --- Runner -------------------------------------------------------------------
[runner]
max_concurrent = 4                   # max simultaneous jobs
# extra_labels = ["gpu", "large"]    # additional labels for runner registration
# default_image = ""                 # override default container image per platform
# job_timeout = "2h"                 # max duration per job
# shutdown_timeout = "5m"            # grace period for running jobs on shutdown

# --- Linux VM (Windows/macOS hosts only) --------------------------------------
[vm.linux]
# enabled = false                    # spin up a Linux VM for cross-OS Linux jobs
# cpus = 2                           # virtual CPUs
# memory_mb = 2048                   # memory in MB
# disk_size_gb = 50                  # sparse disk size in GB

# --- macOS VM (macOS hosts only) ----------------------------------------------
[vm.macos]
# disk_image = ""                    # path to pre-installed macOS VM disk, or
#                                    # auto-pulled from Tart OCI registry
# cpus = 4                           # CPUs per VM
# memory_mb = 8192                   # memory per VM in MB
# max_concurrent = 0                 # max simultaneous macOS VMs (0 = auto-detect)

# --- Networking ---------------------------------------------------------------
[network]
# subnet = ""                        # container subnet (auto-selected if empty)
# mtu = 0                            # bridge MTU (auto-detected from host if 0)

# --- Docker-in-Docker --------------------------------------------------------
[dind]
# enabled = false                    # mount fake Docker socket into containers

# --- Metrics ------------------------------------------------------------------
[metrics]
# enabled = false                    # expose Prometheus /metrics endpoint
# port = 9090                        # metrics listen port
# path = "/metrics"                  # metrics endpoint path

# --- Logging ------------------------------------------------------------------
[log]
level = "info"                       # debug, info, warn, error
format = "text"                      # text or json
# log_retention = "7d"               # max age for job log files (e.g. "7d", "24h")
```

---

## Section reference

### `[github]`

GitHub Actions provider configuration. This is the default provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `owner` | string | **required** | GitHub organization or user name |
| `repos` | string array | `[]` | Limit to specific repos. Omit for org-level runners. |
| `token` | string | `$GITHUB_TOKEN` | Personal access token. Falls back to `GITHUB_TOKEN` env var. |
| `app_id` | integer | -- | GitHub App ID (alternative to PAT auth) |
| `installation_id` | integer | -- | GitHub App installation ID (required with `app_id`) |
| `private_key_path` | string | -- | Path to GitHub App private key PEM file (required with `app_id`) |
| `poll_interval` | string | `"30s"` | How often to poll for queued jobs |

Authentication requires either `token` (or `GITHUB_TOKEN` env var) or all three GitHub App fields (`app_id`, `installation_id`, `private_key_path`).

### `[forgejo]`

Forgejo Actions provider. Setting `instance_url` activates this provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `instance_url` | string | -- | Forgejo instance URL (e.g., `https://codeberg.org`) |
| `token` | string | **required** | Runner registration token from Forgejo admin |
| `owner` | string | `""` | Organization or user. Empty for instance-level runners. |
| `repos` | string array | `[]` | Limit to specific repos. Empty for all repos. |
| `job_image` | string | `"gitea/runner-images:ubuntu-24.04"` | Default job execution image |

### `[gitea]`

Gitea Actions provider. Setting `instance_url` activates this provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `instance_url` | string | -- | Gitea instance URL (e.g., `https://gitea.example.com`) |
| `token` | string | **required** | Runner registration token from Gitea admin |
| `owner` | string | `""` | Organization or user. Empty for instance-level runners. |
| `repos` | string array | `[]` | Limit to specific repos. Empty for all repos. |
| `job_image` | string | `"gitea/runner-images:ubuntu-24.04"` | Default job execution image |

### `[gitlab]`

GitLab CI provider. Setting `instance_url` activates this provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `instance_url` | string | -- | GitLab instance URL (e.g., `https://gitlab.com`) |
| `token` | string | **required** | Runner authentication token (`glrt-xxx` format for GitLab 16+) |
| `tags` | string array | `[]` | Runner tags for job matching |

### `[woodpecker]`

Woodpecker CI provider. Setting `server_url` activates this provider.

| Field | Type | Default | Description |
|---|---|---|---|
| `server_url` | string | -- | Woodpecker server gRPC URL (e.g., `woodpecker.example.com:9000`) |
| `agent_secret` | string | **required** | Shared secret for agent authentication |

### `[webhook]`

Webhook delivery and tunnel configuration. By default, ephemerd polls for jobs. Enable a tunnel for instant webhook delivery.

| Field | Type | Default | Description |
|---|---|---|---|
| `tunnel` | string | `"none"` | `"none"` (polling), `"localtunnel"`, or `"ngrok"` |
| `tunnel_url` | string | `""` | Self-hosted localtunnel server URL |
| `ngrok_authtoken` | string | `""` | ngrok auth token (or use `NGROK_AUTHTOKEN` env var) |
| `secret` | string | auto-generated | Webhook HMAC secret. Auto-generated when a tunnel is active. |
| `port` | integer | `8080` | Listen port for webhook and health endpoint |
| `tls_cert` | string | `""` | TLS certificate path (for direct TLS without a tunnel) |
| `tls_key` | string | `""` | TLS private key path |

### `[runner]`

Job execution settings.

| Field | Type | Default | Description |
|---|---|---|---|
| `max_concurrent` | integer | `4` | Maximum simultaneous jobs |
| `extra_labels` | string array | `[]` | Additional labels for runner registration (e.g., `["gpu"]`) |
| `default_image` | string | platform-specific | Override the default container image |
| `job_timeout` | string | `"2h"` | Maximum duration per job |
| `shutdown_timeout` | string | `"5m"` | Grace period for running jobs during shutdown |

Default images when `default_image` is not set:
- **Linux:** `ghcr.io/actions/actions-runner:latest`
- **Windows:** `mcr.microsoft.com/windows/servercore:ltsc20XX` (auto-detected from host build)

**VM resource planning (Windows and macOS):** On Windows and macOS, `max_concurrent` applies to the entire ephemerd instance — Linux container jobs and native OS jobs share the same concurrency pool. All Linux jobs run inside a single VM (WSL2 on Windows, Virtualization.framework on macOS), so if `max_concurrent = 4`, that VM could be running 4 jobs simultaneously. Size the VM's CPU and memory (`[vm.linux]`) accordingly, or jobs will compete for resources and slow each other down.

### `[vm.linux]`

Linux VM for running Linux jobs on Windows or macOS hosts.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Enable the Linux VM |
| `cpus` | integer | `2` | Virtual CPUs assigned to the VM |
| `memory_mb` | integer | `2048` | Memory in MB |
| `disk_size_gb` | integer | `50` | Sparse disk size in GB |

On Windows, this creates a WSL2 distro with an embedded rootfs. On macOS, it uses Virtualization.framework.

### `[vm.macos]`

macOS VM configuration for running macOS jobs (macOS hosts only). macOS jobs always run in per-job VMs -- there is no toggle to disable this on darwin hosts.

| Field | Type | Default | Description |
|---|---|---|---|
| `disk_image` | string | `""` | Path to a pre-installed macOS VM disk, or auto-pulled from Tart OCI registry |
| `cpus` | integer | `4` | CPUs per VM |
| `memory_mb` | integer | `8192` | Memory per VM in MB |
| `max_concurrent` | integer | auto-detected | Maximum simultaneous macOS VMs. Defaults to auto-detection from host CPU count. |

### `[network]`

Container networking configuration.

| Field | Type | Default | Description |
|---|---|---|---|
| `subnet` | string | auto-selected | Container subnet CIDR. Auto-selected from a private range if empty. |
| `mtu` | integer | auto-detected | Bridge MTU. Auto-detected from the host's default interface if `0`. |

### `[dind]`

Docker-in-Docker support.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Mount a fake Docker socket (`/var/run/docker.sock`) into job containers |

### `[metrics]`

Prometheus metrics endpoint.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Enable the `/metrics` endpoint |
| `port` | integer | `9090` | Metrics listen port |
| `path` | string | `"/metrics"` | Metrics endpoint path |

### `[log]`

Logging configuration.

| Field | Type | Default | Description |
|---|---|---|---|
| `level` | string | `"info"` | Log level: `debug`, `info`, `warn`, `error` |
| `format` | string | `"text"` | Log format: `text` or `json` |
| `log_retention` | string | `"7d"` | Max age for job log files. Supports Go durations (`"168h"`) and day shorthand (`"7d"`). |
