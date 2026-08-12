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
# cache_prune_interval = "24h"       # how often the per-repo image cache pruner runs
# cache_max_age        = "168h"      # evict cached image records inactive longer than this (7 days)

# --- Package caching proxies --------------------------------------------------
# [module_proxy]
# enabled  = false                   # run a GOPROXY on the bridge gateway
# port     = 8082                    # listen port
# upstream = "https://proxy.golang.org"
# cleanup  = true                    # wipe the cache on shutdown

# [cargo_proxy]
# enabled         = false            # pull-through cache for crates.io + rustup
# port            = 8083             # listen port
# upstream        = "https://index.crates.io"
# rustup_upstream = "https://static.rust-lang.org"
# index_ttl       = "10m"            # sparse-index revalidation interval
# cleanup         = false            # keep the cache across restarts

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

**VM resource planning (Windows and macOS):** On Windows and macOS, `max_concurrent` applies to the entire ephemerd instance — Linux container jobs and native OS jobs share the same concurrency pool. All Linux jobs run inside a single VM (Hyper-V Linux VM on Windows, Virtualization.framework on macOS), so if `max_concurrent = 4`, that VM could be running 4 jobs simultaneously. Size the VM's CPU and memory (`[vm.linux]`) accordingly, or jobs will compete for resources and slow each other down.

### `[vm.linux]`

Linux VM for running Linux jobs on Windows or macOS hosts.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Enable the Linux VM |
| `cpus` | integer | `2` | Virtual CPUs assigned to the VM |
| `memory_mb` | integer | `2048` | Memory in MB |
| `disk_size_gb` | integer | `50` | Sparse disk size in GB |

On Windows, this creates a Hyper-V Linux VM via the HCS (Host Compute Service) API, booted from an embedded kernel + initrd onto a persistent VHDX. On macOS, it uses Virtualization.framework.

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

Docker-in-Docker support. When `enabled`, every job sees `/var/run/docker.sock` and the runner's containerd serves a fake Docker Engine API on it. Image pulls from inside the job (e.g. `kind create cluster` pulling `kindest/node`) are mirrored into a long-lived per-repo namespace so the next job in the same repo gets a content-store hit instead of re-pulling.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Mount a fake Docker socket (`/var/run/docker.sock`) into job containers |
| `cache_prune_interval` | duration | `"24h"` | How often the per-repo image cache pruner runs. Set to `"0"` to disable pruning. |
| `cache_max_age` | duration | `"168h"` (7d) | Evict cached image records whose `ephemerd.io/last-accessed` label is older than this. Containerd's content GC reclaims the now-unreferenced blobs. |

**Per-repo image cache.** Each (provider, repo) pair gets its own long-lived containerd namespace named `ephemerd-dind-cache-<provider>-<sanitized-repo>`. Examples:

```
ephemerd-dind-cache-github-ephpm_ephpm
ephemerd-dind-cache-gitea-ephpm_ephpm        ← distinct from the github one
ephemerd-dind-cache-gitlab-acme_platform_api ← nested GitLab groups OK
```

The cache namespace persists across jobs and across ephemerd restarts. Per-job state lives in a separate namespace (`ephemerd-dind-<runner-name>`) which is deleted when each job exits.

**Privacy boundary.** Containerd namespace isolation prevents one repo's cached image blobs from being resolved by any other namespace. Two forges with identically-named repos (`github/foo` vs `gitea/foo`) do not share a cache. Two repos within the same forge do not share a cache. Auth credentials are scoped to the per-job namespace's in-memory auth cache and are never copied into the long-lived cache namespace.

**Pruning.** Every `cache_prune_interval`, dind walks each `ephemerd-dind-cache-*` namespace and evicts Image records whose `ephemerd.io/last-accessed` label is older than `cache_max_age`. Cache namespaces left empty after eviction are removed entirely. Records pre-dating the label fall back to the record's `UpdatedAt` timestamp so a deploy that introduces the cache feature doesn't nuke pre-existing records on first prune.

**Disabling caching.** Setting `cache_max_age = "0"` disables eviction (the cache grows unbounded — useful for debugging but not recommended in production). Setting `cache_prune_interval = "0"` disables the pruner goroutine entirely; equivalent to "keep everything forever, even empty namespaces."

### `[module_proxy]`

Go module caching proxy. ephemerd runs a single GOPROXY on the bridge gateway and injects `GOPROXY=http://<gateway>:<port>|direct` into every job container, so repeated `go mod download` runs hit the local disk cache instead of `proxy.golang.org`.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Run the Go module proxy |
| `port` | integer | `8082` | Listen port on the bridge gateway |
| `upstream` | string | `"https://proxy.golang.org"` | Fetched from on a cache miss |
| `cleanup` | boolean | `true` | Wipe the cache directory on shutdown. Set `false` to keep it across restarts. |

Immutable module files (`.info`, `.mod`, `.zip`) are cached; mutable endpoints (`@latest`, `@v/list`) and `sumdb` requests pass through. The `|direct` separator means the go command falls back to the origin on **any** proxy error, so a broken cache slows a build rather than failing it.

### `[cargo_proxy]`

Cargo/crates caching proxy — the Rust counterpart to `[module_proxy]`. One HTTP server on the bridge gateway serves three routes:

| Route | Upstream | Caching |
|---|---|---|
| `/index/…` | `upstream` (sparse registry index) | **Mutable** — served from cache for `index_ttl`, then revalidated with a conditional GET |
| `/crates/{name}/{version}/download` | the registry's own `dl` template | **Immutable** — cached permanently, never refetched |
| `/rustup/…` | `rustup_upstream` | Dated artifacts (`dist/YYYY-MM-DD/…`) immutable; channel manifests revalidated on `index_ttl` |

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Run the Cargo proxy |
| `port` | integer | `8083` | Listen port on the bridge gateway |
| `upstream` | string | `"https://index.crates.io"` | Sparse registry index |
| `rustup_upstream` | string | `"https://static.rust-lang.org"` | Toolchain distribution server |
| `index_ttl` | duration | `"10m"` | How long a cached index entry is served before revalidation. A negative value revalidates on every request. |
| `cleanup` | boolean | `false` | Wipe the cache on shutdown. Defaults to **false**, unlike `[module_proxy]` — a pull-through cache that empties itself on every restart saves nothing. |

**How jobs pick it up — no workflow changes required.**

- **rustup** reads its mirror from the environment, so ephemerd injects `RUSTUP_DIST_SERVER`.
- **Cargo does not.** Source replacement (`[source.crates-io] replace-with`) is the only mechanism Cargo offers for redirecting crates.io, and it is read **exclusively from config files** — `CARGO_SOURCE_*` environment variables are silently ignored. ephemerd therefore generates a `.cargo/config.toml` under `<data-dir>/cargo/` and bind-mounts it **read-only** at the container's filesystem root (`/.cargo`, or `C:\.cargo` on Windows). Cargo searches the current directory and *every ancestor* for `.cargo/config.toml`, so a file at the root applies to any workspace path a job checks out — no knowledge of the checkout location, the job user's home, or the image's `CARGO_HOME` is needed, and `CARGO_HOME` itself is left untouched so it stays writable.

A repository that ships its own `.cargo/config.toml` still wins: Cargo prefers config closer to the workspace.

**Fail-open behaviour.** A cache must never turn a registry hiccup into a red CI job, so failures degrade in three layers:

1. If the proxy does not start, nothing is injected and no mount is added — jobs go straight to crates.io.
2. If an upstream is unreachable or returns 5xx and a cached copy exists, the **stale copy is served** with a warning.
3. If nothing is cached, crate and rustup downloads are answered with a `307` redirect to the real origin, so the job fetches it directly (slower, uncached) instead of failing.

Genuine `404`s are passed through as `404` — a nonexistent crate version must stay distinguishable from an outage.

**Scope.** The mount lands in the runner container. Jobs that run their steps inside a *further* container (a `container:` image spawned via Docker-in-Docker) do not inherit it.

**Cache location.** Cached content lives at `<data-dir>/cache/cargo/` and is visible to `ephemerd cache list` / `ephemerd cache clear cargo`. The generated container config lives at `<data-dir>/cargo/` — deliberately outside the cache root, so clearing the cache cannot pull the mounted config out from under a running job.

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
