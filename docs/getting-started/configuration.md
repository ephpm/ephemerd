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
# cache_prune_interval = "24h"       # how often empty per-repo cache namespaces are reaped
# cache_max_age        = "0"         # OPTIONAL age backstop for the dind cache (0 = off; see [image_gc])

# --- BuildKit build cache -----------------------------------------------------
[buildkit]
# gc_enabled                   = true    # bound the build cache (leave on)
# gc_reserved_gb               = 5       # never collected, even when idle (warm floor)
# gc_max_used_gb               = 25      # hard ceiling on total build cache
# gc_min_free_gb               = 20      # collect to keep at least this much disk free
# gc_keep_duration             = "168h"  # collect records idle longer than this, above the floor
# gc_ephemeral_keep_duration   = "48h"   # cheap-to-rebuild records (contexts, cache mounts, git checkouts)
# gc_ephemeral_max_used_gb     = 2

# --- Disk-pressure image GC ---------------------------------------------------
[image_gc]
# enabled                 = true   # evict container images when the disk fills
# check_interval          = "60s"  # one statfs per tick; cheap
# high_watermark_percent  = 85     # start collecting at this disk usage
# low_watermark_percent   = 70     # collect down to this, then stop
# min_free_gb             = 20     # absolute floor; triggers regardless of percentage
# target_free_gb          = 40     # free space a floor-triggered pass restores (default 2x min_free_gb)
# max_age                 = "0"    # OPTIONAL age backstop across all namespaces (0 = off)

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
| `enabled` | boolean | platform-dependent | Enable the Linux VM. When unset: `true` on macOS, `false` on Windows (opt-in). Meaningless on Linux hosts, where jobs run natively. |
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
| `l2bridge_egress` | boolean | `false` | **Windows only.** Enforce Windows container egress filtering. The default Windows NAT network **cannot** be egress-filtered by any host-side mechanism; this puts containers on an HNS L2Bridge with per-endpoint VFP ACLs, the only path that enforces. Requires `host_nic` and `ip_pool`. See [Security → Network Firewall](../guides/security.md#network-firewall). |
| `host_nic` | string | none | **Windows, required when `l2bridge_egress = true`.** Host adapter to bridge onto (as shown by `Get-NetAdapter`). A dedicated NIC is strongly recommended — see the Security guide. |
| `ip_pool` | string | none | **Windows, required when `l2bridge_egress = true`.** LAN range ephemerd assigns to job containers (CIDR or `start-end`). **No default** — reserve it in your DHCP scope *before* enabling, or containers collide with live leases. |

> **Windows egress is not filtered by default.** On the default NAT network, ephemerd installs no egress filter and logs that egress is unfiltered. To enforce it, set `l2bridge_egress` (plus `host_nic` and `ip_pool`) — the full procedure and traps are in the [Security guide's Network Firewall section](../guides/security.md#network-firewall). Linux and macOS jobs are egress-filtered by default.

### `[dind]`

Docker-in-Docker support. When `enabled`, every job sees `/var/run/docker.sock` and the runner's containerd serves a fake Docker Engine API on it. Image pulls from inside the job (e.g. `kind create cluster` pulling `kindest/node`) are mirrored into a long-lived per-repo namespace so the next job in the same repo gets a content-store hit instead of re-pulling.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Mount a fake Docker socket (`/var/run/docker.sock`) into job containers |
| `cache_prune_interval` | duration | `"24h"` | How often the per-repo cache reaper runs. It removes cache namespaces left with no image records, and applies `cache_max_age` when that is set. `"0"` disables the loop. |
| `cache_max_age` | duration | `"0"` (off) | **Optional** age backstop: evict cached image records whose `ephemerd.io/last-accessed` label is older than this. |

> **Behavior change.** `cache_max_age` used to default to `"168h"` and was the only image eviction ephemerd performed. It now defaults to **off**. Disk pressure — not age — is the trigger; see [`[image_gc]`](#image_gc). Age-based eviction throws away a warm cache while the disk is half empty and forces re-downloads, which is the opposite of what a bandwidth-constrained node needs. An explicit `cache_max_age` is still honored and still applies only to the `ephemerd-dind-cache-*` namespaces.

**Per-repo image cache.** Each (provider, repo) pair gets its own long-lived containerd namespace named `ephemerd-dind-cache-<provider>-<sanitized-repo>`. Examples:

```
ephemerd-dind-cache-github-ephpm_ephpm
ephemerd-dind-cache-gitea-ephpm_ephpm        ← distinct from the github one
ephemerd-dind-cache-gitlab-acme_platform_api ← nested GitLab groups OK
```

The cache namespace persists across jobs and across ephemerd restarts. Per-job state lives in a separate namespace (`ephemerd-dind-<runner-name>`) which is deleted when each job exits.

**Privacy boundary.** Containerd namespace isolation prevents one repo's cached image blobs from being resolved by any other namespace. Two forges with identically-named repos (`github/foo` vs `gitea/foo`) do not share a cache. Two repos within the same forge do not share a cache. Auth credentials are scoped to the per-job namespace's in-memory auth cache and are never copied into the long-lived cache namespace.

**Pruning.** Every `cache_prune_interval`, dind walks each `ephemerd-dind-cache-*` namespace and evicts Image records whose `ephemerd.io/last-accessed` label is older than `cache_max_age`. Cache namespaces left empty after eviction are removed entirely. Records pre-dating the label fall back to the record's `UpdatedAt` timestamp so a deploy that introduces the cache feature doesn't nuke pre-existing records on first prune.

**Disabling caching.** Setting `cache_prune_interval = "0"` disables the reaper goroutine entirely; equivalent to "keep everything forever, even empty namespaces." Cache size itself is bounded by `[image_gc]`, not by this loop.

### `[buildkit]`

Bounds the embedded BuildKit solver's on-disk build cache.

| Field | Type | Default | Description |
|---|---|---|---|
| `gc_enabled` | boolean | `true` | Garbage-collect the build cache. `false` restores the old unbounded behavior. |
| `gc_reserved_gb` | integer | `5` | Cache never collected, even when idle — the warm floor. |
| `gc_max_used_gb` | integer | `25` | Hard ceiling; anything above it is collected regardless of age. |
| `gc_min_free_gb` | integer | `20` | Collect whatever is needed to keep this much disk free, overriding the floor. Matches `[image_gc].min_free_gb`. |
| `gc_keep_duration` | duration | `"168h"` (7d) | Age past which records are collected once usage exceeds the floor. |
| `gc_ephemeral_keep_duration` | duration | `"48h"` | Age limit for cheaply reproducible records (local build contexts, `RUN --mount=cache` mounts, git checkouts). |
| `gc_ephemeral_max_used_gb` | integer | `2` | Ceiling for those same records. |

> **Why this table exists.** BuildKit only garbage-collects when its worker is given a GC policy — its controller is literally `if len(policy) > 0 { prune }`. ephemerd never supplied one, so every `docker build` in every CI job added cache records, snapshots and `containerd.io/gc.flat` leases to the shared `buildkit` containerd namespace that nothing ever released. A production node accumulated 76 image records, 302 snapshots and 481 leases — roughly 44 GB of a 116 GB disk — across 49 dead jobs over two and a half weeks, and that is what filled the disk until QEMU froze the VM.

Separately, each job's `docker build` output is exported into that shared namespace under a name scoped by the job's unique ID (`build.ephemerd.local/<job-id>/<tag>`), so concurrent jobs cannot race on the same tag. Because the ID is never reused, those records are garbage the moment the job ends. They are removed at job teardown and swept periodically for jobs lost to a crash.

### `[image_gc]`

Disk-pressure-triggered container image garbage collection, covering the `buildkit` namespace, the main `ephemerd` runtime namespace, and the per-repo `ephemerd-dind-cache-*` namespaces.

**Disk pressure is the trigger; least-recently-used is the order** — kubelet's model. Collection starts when a watermark is crossed and evicts LRU-first until a distinctly lower one is reached, then stops. Two watermarks rather than one line is what prevents thrashing at the boundary.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `true` | Run image garbage collection. |
| `check_interval` | duration | `"60s"` | How often disk usage is sampled. One `statfs`/`GetDiskFreeSpaceExW` call — microseconds, never a directory walk. `"0"` disables the periodic sweep; the pre-pull check still runs. |
| `high_watermark_percent` | float | `85` | Disk used-percentage at which a pass triggers. |
| `low_watermark_percent` | float | `70` | Used-percentage a triggered pass evicts down to. |
| `min_free_gb` | integer | `20` | Absolute free-space floor; triggers regardless of percentage. |
| `target_free_gb` | integer | `2 x min_free_gb` | Free space a floor-triggered pass restores. |
| `max_age` | duration | `"0"` (off) | Optional age backstop across every collected namespace. |

**Two trigger arms, most conservative wins.** Neither is safe alone. 15% free of a 1 TB node is 150 GB and evicting there is pointless churn; 15% free of a 100 GB node is 15 GB, which three concurrent jobs writing ~5 GB of container layers each can consume between ticks. Size `min_free_gb` relative to `runner.max_concurrent` times the expected per-job writable layer.

**Never evicted.** Images referenced by any existing container in any namespace; the node's configured runner images (`[runner].default_image`, every `[runner.images.<repo>]` entry, and each provider's `default_image_*`) — dropping one of those guarantees a re-pull on the very next job; and any live job's BuildKit export records, which no container references and so would otherwise look unused.

**Trigger points.** The `check_interval` timer, and inline before pulling an image or creating a runner environment. The timer alone loses a race a single job can win: one multi-gigabyte toolchain pull can cross the high watermark well inside a 60s tick.

**Failsafe.** If a pass evicts everything it is allowed to and usage is still above the high watermark, the remainder is live data, not cache. ephemerd logs that at ERROR with the numbers and then suppresses further passes for 30 minutes rather than spinning.

**LRU key.** Eviction order comes from the `ephemerd.io/last-accessed` label, refreshed on pull, import and container start. Records pre-dating the label fall back to containerd's `UpdatedAt`, so a node upgrading into this feature sorts sanely instead of treating everything as never-used.

**Orphan sweep.** The same timer runs a job-safe orphan sweep (leftover per-job runner-dir copies, job workdirs, and container snapshots with no owning container). This previously ran only at startup, so a long-lived daemon accumulated them for its entire uptime.

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
