# ephemerd Architecture

A cross-platform daemon for managing ephemeral GitHub Actions self-hosted runners. Single Go binary that embeds containerd as a library (like k3s) and spins up isolated, disposable runner environments for each CI job.

## Problem

No existing solution manages ephemeral GitHub Actions runners across both Linux and Windows from a single control plane:

- **ARC (Actions Runner Controller)** -- Kubernetes-only, Linux-only, no Windows support
- **Firecracker-based** (fireactions, appsignal) -- Linux-only microVMs
- **GitHub hosted runners** -- no ARM64 Windows, limited ARM64 Linux, expensive macOS, no environment control
- **Community Windows containers** -- manual Docker setups, no orchestration, no real isolation

Self-hosted runners on bare metal are insecure for public repos -- any PR can run arbitrary code on the host. ephemerd solves this by running every job in an ephemeral, isolated environment that is destroyed after the job completes.

## Why Go

The entire ecosystem ephemerd integrates with is Go:

- **containerd** -- Go library, designed to be imported directly (k3s/rke2 proved this)
- **GitHub Actions runner scale set client** -- Go module
- **OCI/container specs** -- Go reference implementations

By writing ephemerd in Go, containerd runs in-process as a library -- no binary embedding, no child process management, no extract-and-spawn lifecycle. Direct access to containerd's internal APIs for snapshots, tasks, namespaces, and image management. One binary, one process, no version mismatches.

## Design

### Core Loop

1. Register with GitHub as a runner scale set (or poll for jobs via webhook)
2. Receive job assignment
3. Provision an ephemeral environment (container or VM) from a pre-built image
4. Install and start the GitHub Actions runner inside the environment
5. Job executes in full isolation from the host
6. On completion (success or failure), tear down the environment -- clean slate

### Embedded containerd

Following the k3s/rke2 model, ephemerd imports containerd as a Go library and runs it in-process. No external containerd install, no system service, no socket management, no version mismatches.

**How k3s does it (and how we follow):**

- Import `github.com/containerd/containerd/v2` packages directly
- Start containerd's server components in a goroutine within the ephemerd process
- containerd's gRPC services are available in-process (no Unix socket needed, though one can be exposed for debugging)
- Snapshotter, content store, and image service all run in the same process
- On shutdown, containerd tears down cleanly with the parent process

**What this gives us:**

- Single binary deployment -- `ephemerd` is all you install
- No separate containerd service to configure, upgrade, or monitor
- No socket permissions issues
- Direct Go API access instead of gRPC round-trips for internal operations
- Consistent containerd version across all deployments

**Data directory layout:**

```
/var/lib/ephemerd/              # Linux / macOS
C:\ProgramData\ephemerd\        # Windows
  containerd/
    state/                      # containerd runtime state
    root/                       # image store, snapshots
  runners/                      # ephemeral runner workdirs (cleaned per job)
  vm/                           # macOS only: Linux VM kernel + initrd cache
  config.toml                   # ephemerd config
```

### Isolation Model

containerd manages OCI images and container lifecycle on every platform. The isolation mechanism differs by host OS, but the image format is always OCI — one Dockerfile builds images that run everywhere.

#### Linux: containerd containers (direct)

- Standard OCI containers via embedded containerd, running directly on the host kernel
- Supports x86_64 and aarch64
- Fast startup (~1s)
- Optional Firecracker microVM backend for stronger isolation (future)

#### Windows: containerd + Hyper-V isolation

- containerd runs natively on Windows and supports Hyper-V isolation
- Each container gets its own kernel in a lightweight VM — real isolation, malicious code cannot escape to the host
- Same OCI images, same containerd APIs, just compiled for Windows
- Supports x86_64 (aarch64 when ecosystem matures)
- Startup ~5-10s (acceptable for CI jobs running 20+ minutes)

#### macOS: Linux-on-Mac via Virtualization.framework

macOS cannot run OCI containers natively — there are no namespaces, cgroups, or union filesystems. Every tool that runs containers on macOS (Docker Desktop, Colima, etc.) uses a hidden Linux VM. ephemerd does the same, but explicitly and efficiently.

**How it works:**

1. On startup, ephemerd boots a lightweight Linux VM using Apple's Virtualization.framework (built into macOS 12+, no third-party deps)
2. containerd runs inside the Linux VM, managed by ephemerd
3. The VM stays running as long as ephemerd is running — it's the container host
4. OCI images are pulled and cached by containerd inside the VM
5. Per-job containers run inside the VM, isolated from both the VM and the macOS host

**OCI image unpacking into the VM:**

When a job is queued, ephemerd uses containerd (inside the VM) to pull the OCI image if not cached, create a container from it, and run the job — identical to what happens on a native Linux host. The VM is transparent to the job.

Pre-built tools (libphp, Rust toolchain, etc.) are baked into OCI images via standard Dockerfiles. The same image that runs on a Linux x86_64 host runs on a Mac's Linux VM — just ARM64 if the Mac is Apple Silicon.

**Why this works for homelab:**

- A single Mac Mini serves as an ARM64 Linux runner — no separate ARM64 Linux hardware needed
- Same OCI images and Dockerfiles as every other platform
- Virtualization.framework is hardware-accelerated on Apple Silicon — near-native performance
- The Linux VM is lightweight (~256MB RAM base) and boots in seconds

**What this does NOT support:**

- macOS-native jobs (Xcode, Swift, iOS simulator, code signing) — these require a macOS VM, which is a different image format (IPSW/disk snapshot) and a different provisioning workflow. This is out of scope for the initial release. If needed, users can provision macOS VMs separately using Tart or similar tools.

### Dual-purpose hosts

Because Windows can run Hyper-V Linux VMs and macOS can run Virtualization.framework Linux VMs, a single machine can serve multiple job types:

| Host | Linux jobs | Native OS jobs |
|------|-----------|----------------|
| Linux x86_64 | containerd (direct) | — |
| Linux arm64 | containerd (direct) | — |
| Windows x86_64 | containerd in Hyper-V Linux VM | Hyper-V Windows containers |
| macOS arm64 | containerd in Virtualization.framework Linux VM | — (future: macOS VMs) |

A Windows box and a Mac Mini together cover every combination: linux/amd64, linux/arm64, windows/amd64.

### Build Matrix

Each OS/arch combination produces one self-contained binary with containerd compiled in:

| Target | Binary | How it runs containers |
|--------|--------|----------------------|
| linux/amd64 | `ephemerd` | containerd direct |
| linux/arm64 | `ephemerd` | containerd direct |
| windows/amd64 | `ephemerd.exe` | containerd + Hyper-V (Windows jobs) / Hyper-V Linux VM (Linux jobs) |
| darwin/arm64 | `ephemerd` | Virtualization.framework Linux VM + containerd inside |

No runtime dependencies beyond the OS kernel, Hyper-V (Windows), or Virtualization.framework (macOS).

### GitHub Integration

Two options for receiving jobs:

**Option A: Polling (default)**
- Check GitHub API every N seconds for queued workflow jobs
- Zero inbound port requirements — works behind NAT, no TLS certs needed
- Simple to set up, ideal for homelab

**Option B: Webhook + JIT Runners**
- Listen for `workflow_job` webhook events over TLS
- Instant job delivery, no polling delay
- Requires inbound port + TLS certificate

Start with polling for simplicity. Enable webhooks by adding TLS cert/key to config.

### Image Management

Pre-built OCI images per platform with common build tools, stored in any container registry:

**Linux images (run on all hosts):**
- `ephemerd/build` -- gcc, cmake, autoconf, make, pkg-config, Rust toolchain
- `ephemerd/build-php` -- above + PHP build dependencies for spc

**Windows images (run on Windows hosts only):**
- `ephemerd/build-windows` -- MSVC Build Tools, cmake, git, 7-zip
- `ephemerd/build-windows-php` -- above + PHP SDK binary tools, spc dependencies

Images are pulled/cached by the embedded containerd. Same `docker build` / `docker push` workflow users already know.

### Configuration

Single TOML config file:

```toml
[github]
# Authentication: personal access token or GitHub App
token = "ghp_..."
# Or:
# app_id = 12345
# private_key_path = "/etc/ephemerd/github-app.pem"

# Which org/user and repos to register runners for
owner = "ephpm"
repos = ["ephpm", "php-sdk", "litewire"]

# Job discovery: polling (default) or webhook
poll_interval = "10s"
# Webhook mode (optional): set tls_cert + tls_key to enable
# webhook_port = 8080
# webhook_secret = "your_secret"
# tls_cert = "/etc/ephemerd/tls.crt"
# tls_key = "/etc/ephemerd/tls.key"

[runner]
default_image = "ghcr.io/ephpm/ephemerd-build:latest"
max_concurrent = 4
extra_labels = []
job_timeout = "2h"
shutdown_timeout = "5m"

[log]
level = "info"    # debug, info, warn, error
format = "text"   # text or json
```

### Runner Labels

ephemerd auto-applies labels based on the environment:

- `self-hosted`
- OS: `linux` or `windows`
- Arch: `x64` or `arm64`
- Custom labels from config

Workflows select runners as usual:

```yaml
runs-on: [self-hosted, linux, x64]      # Linux container (any host)
runs-on: [self-hosted, linux, arm64]     # Linux container (arm64 Linux or Mac host)
runs-on: [self-hosted, windows, x64]     # Hyper-V Windows container
```

## Known Limitations

### Windows: No `services:` or `container:` YAML Keys

GitHub's runner binary blocks container operations (`services:`, `container:`) on Windows. This is hardcoded in the runner, not something ephemerd can fix.

**Workaround:** Use `docker run` directly in job steps to start sidecars:

```yaml
# Instead of services: { mysql: { image: mysql:8 } }
- name: Start MySQL
  run: docker run -d --name mysql -p 3306:3306 -e MYSQL_ROOT_PASSWORD=test mysql:8
- name: Run tests
  run: php test.php
- name: Stop MySQL
  run: docker stop mysql
```

This is functionally identical — `services:` is syntactic sugar. Document this for users and move on.

### macOS: No macOS-native jobs (initial release)

ephemerd on macOS runs Linux jobs inside a Virtualization.framework VM. macOS-native jobs (Xcode, Swift, code signing) require macOS VM snapshots with a completely different provisioning pipeline. This is deferred — use Tart or Anka for macOS-native CI if needed.

### ARM64 Windows

PHP and its toolchain don't support Windows ARM64 yet. ephemerd can support it at the infrastructure level (Hyper-V containers work on ARM64 Windows), but there's nothing useful to run in them until the ecosystem catches up.

## Tech Stack

- **Language:** Go
- **containerd:** imported as library (github.com/containerd/containerd/v2), runs in-process
- **macOS VM:** Apple Virtualization.framework via `Code-Hex/vz` Go bindings
- **GitHub API:** go-github + runner scale set client module
- **Config:** TOML (BurntSushi/toml)
- **Logging:** slog (stdlib structured logging)
- **CLI:** cobra

## Project Structure

```
ephemerd/
  cmd/
    ephemerd/
      main.go             -- CLI entry point, config loading, daemon lifecycle
  pkg/
    config/
      config.go           -- Configuration structs, TOML parsing
    containerd/
      server.go           -- Embedded containerd server lifecycle
    github/
      client.go           -- GitHub API client, webhook/polling
      runner.go           -- JIT runner registration/deregistration
    runtime/
      runtime.go          -- Backend interface: Create/Wait/Destroy
      containerd.go       -- Linux/Windows: direct containerd containers
      hyperv.go           -- Windows: Hyper-V isolation options
      vm.go               -- macOS: Virtualization.framework Linux VM
    networking/
      networking.go       -- CNI bridge (Linux), HCN NAT (Windows), VM NAT (macOS)
    runner/
      embed.go            -- Embedded GitHub Actions runner binary (go:embed)
      extract.go          -- Extract/cache runner to data dir
    scheduler/
      scheduler.go        -- Job queue, concurrency limits, lifecycle
  Makefile
  go.mod
  go.sum
```
