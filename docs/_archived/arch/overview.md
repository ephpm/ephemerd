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

**OCI images — same format everywhere:**

An OCI (Open Container Initiative) image is a portable, layered filesystem archive. It's what `docker build` produces — a stack of tarballs, each layer adding files on top of the previous one. A Dockerfile like:

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y build-essential cmake
COPY libphp.a /usr/local/lib/
```

Produces an OCI image with three layers: the Ubuntu base, the build tools, and libphp. This image is pushed to a container registry (Docker Hub, ghcr.io, etc.) and pulled by containerd on any host.

**How the image gets into the macOS Linux VM:**

1. The Linux VM boots with containerd running inside it
2. containerd inside the VM pulls the OCI image from the registry (or uses its local cache)
3. containerd unpacks the image layers into a snapshot — each layer's tarball is extracted and stacked using overlayfs, creating the final root filesystem
4. A container is created from that snapshot with the job's entrypoint (the GitHub Actions runner)
5. The container runs, the job executes, and when it's done the container and snapshot are deleted

This is identical to what happens on a native Linux host. The VM is invisible to the job — the container sees a normal Linux filesystem with all the tools from the image layers already in place. containerd handles all the OCI image pulling, layer unpacking, and snapshot management.

Pre-built tools (libphp, Rust toolchain, etc.) are baked into OCI images via standard Dockerfiles. The same image that runs on a Linux x86_64 host runs on a Mac's Linux VM — just ARM64 if the Mac is Apple Silicon.

**macOS-native jobs use a different model:**

macOS VMs don't use OCI images — they use a provisioned macOS disk snapshot. The base image is created once (install Xcode, Homebrew, etc.), and each job gets an APFS clone-on-write copy. This is instant (no data copied until the job writes to disk) and the clone is deleted when the job completes. See the macOS VM section below.

**Why this works for homelab:**

- A single Mac Mini serves as an ARM64 Linux runner — no separate ARM64 Linux hardware needed
- Same OCI images and Dockerfiles as every other platform
- Virtualization.framework is hardware-accelerated on Apple Silicon — near-native performance
- The Linux VM is lightweight (~256MB RAM base) and boots in seconds

**macOS-native jobs (Xcode, Swift, code signing):**

For jobs that require macOS itself (not just Linux on a Mac), ephemerd boots an ephemeral macOS VM per job using Virtualization.framework:

1. You provision a base macOS disk image once — install Xcode, Homebrew, whatever you need
2. When a macOS-labeled job arrives, ephemerd creates an APFS clone-on-write copy of the base image (instant, no data copied)
3. A macOS VM boots from the clone (~30s, Xcode and all tools already installed)
4. The GitHub/GitLab runner inside the VM picks up the job
5. When the job completes, the VM is stopped and the clone is deleted

This is a different image format than OCI (disk snapshot vs container layers) but the lifecycle is the same: create → run → destroy. Configure with `[vm.macos]` in the config.

### Dual-purpose hosts

Because Windows can run Hyper-V Linux VMs and macOS can run Virtualization.framework Linux VMs, a single machine can serve multiple job types:

| Host | Linux jobs | Native OS jobs |
|------|-----------|----------------|
| Linux x86_64 | containerd (direct) | — |
| Linux arm64 | containerd (direct) | — |
| Windows x86_64 | containerd in Hyper-V Linux VM | Hyper-V Windows containers |
| macOS arm64 | containerd in Virtualization.framework Linux VM | Ephemeral macOS VMs (clone-on-write) |

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

**Option B: Webhook via Tunnel**
- ephemerd creates a tunnel (localtunnel or ngrok) and registers webhooks automatically
- Instant job delivery, no polling delay
- No inbound ports needed — tunnels work behind NAT
- On shutdown, webhooks are deregistered

**Option C: Webhook via Direct TLS**
- For hosts with a public IP and TLS certificate
- Requires inbound port + TLS certificate + manual webhook setup

Start with polling for simplicity. Enable tunnels for instant delivery.

### GitLab Integration

ephemerd supports GitLab CI alongside GitHub Actions. The isolation layer (containerd, VMs, networking, firewall) is CI-system-agnostic — only the job discovery and runner registration differ.

**Architecture: Custom Executor**

GitLab runners support a [custom executor](https://docs.gitlab.com/runner/executors/custom.html) that calls user-defined scripts for each phase of job execution. ephemerd embeds the `gitlab-runner` binary and configures it to use a custom executor that delegates container lifecycle back to ephemerd.

The flow:

1. ephemerd starts with `[gitlab]` config present
2. Extracts the embedded `gitlab-runner` binary
3. Generates a `config.toml` for gitlab-runner with the custom executor pointing to ephemerd's own binary as the handler
4. Launches `gitlab-runner run` in a goroutine — it polls GitLab for jobs
5. When a job arrives, gitlab-runner calls ephemerd's custom executor scripts:
   - **`prepare`** — ephemerd creates an OCI container via containerd (same as GitHub path)
   - **`run`** — ephemerd executes the job step inside the container
   - **`cleanup`** — ephemerd destroys the container and cleans up networking

This gives us the same isolation model as GitHub — ephemeral containers per job, Hyper-V on Windows, Linux VMs on macOS — with GitLab handling job discovery and artifact upload.

**Config:**

```toml
[gitlab]
url = "https://gitlab.com"       # GitLab instance URL
token = "glrt-your-runner-token"  # runner authentication token
tags = ["ephemerd", "linux"]      # runner tags for job matching
concurrent = 4                    # max parallel jobs (mirrors runner.max_concurrent)
```

GitHub and GitLab can run simultaneously on the same ephemerd instance — they share the containerd runtime, VM infrastructure, and networking stack. Jobs are isolated from each other regardless of which CI system dispatched them.

**What this reuses from GitHub:**

- Embedded containerd (in-process)
- Container creation, networking, firewall rules
- Linux VM on macOS/Windows for cross-OS jobs
- macOS VM per-job for native jobs
- Runner image management (same OCI images)
- Graceful shutdown and orphan cleanup

**What's GitLab-specific:**

- `gitlab-runner` binary embedding and lifecycle management
- Custom executor script generation (`prepare`/`run`/`cleanup`)
- Runner registration with GitLab instance
- GitLab-specific runner tags and configuration

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
poll_interval = "30s"
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

### macOS: Implemented

macOS-native jobs are fully supported. Per-job ephemeral macOS VMs boot from a base image via APFS clone-on-write. Base images are automatically pulled from the Tart OCI registry (e.g., `ghcr.io/cirruslabs/macos-tahoe-vanilla:latest`) on first use, or provisioned manually via a disk image path in `[vm.macos] disk_image`.

### ARM64 Windows

PHP and its toolchain don't support Windows ARM64 yet. ephemerd can support it at the infrastructure level (Hyper-V containers work on ARM64 Windows), but there's nothing useful to run in them until the ecosystem catches up.

## Tech Stack

- **Language:** Go
- **containerd:** imported as library (github.com/containerd/containerd/v2), runs in-process
- **macOS VM:** Apple Virtualization.framework via `Code-Hex/vz` Go bindings
- **GitHub API:** go-github + runner scale set client module
- **Config:** TOML (BurntSushi/toml)
- **Logging:** slog (stdlib structured logging)
- **CLI:** urfave-cli/v3

## Project Structure

```
ephemerd/
  cmd/
    ephemerd/
      main.go             -- CLI entry point (urfave-cli/v3), daemon lifecycle
      commands.go         -- Subcommand implementations (jobs, images, ctrctl)
      run.go              -- Local workflow runner (ephemerd run)
      install.go          -- Install/uninstall as system service
      doctor.go           -- System readiness checks
      ssh.go              -- SSH into macOS VM jobs
      service.go          -- Start/stop/restart/logs for system service
  pkg/
    config/
      config.go           -- Configuration structs, TOML parsing
    containerd/
      server.go           -- Embedded containerd server lifecycle
    github/
      client.go           -- GitHub API client
      runner.go           -- JIT runner registration/deregistration
    providers/
      provider.go         -- Multi-forge provider interface
      github/             -- GitHub provider
      forgejo/            -- Forgejo provider
      gitea/              -- Gitea provider
      gitlab/             -- GitLab provider
      woodpecker/         -- Woodpecker CI provider
    runtime/
      runtime.go          -- Container lifecycle: Create/Wait/Destroy
    networking/
      networking.go       -- CNI bridge (Linux), HCN NAT (Windows), VM NAT (macOS)
    runner/
      runner.go           -- Embedded GitHub Actions runner binary (go:embed)
    scheduler/
      scheduler.go        -- Job queue, concurrency limits, lifecycle
      dispatch.go         -- gRPC dispatch for WSL Linux jobs
    tunnel/               -- Webhook tunnel providers (localtunnel, ngrok)
    dind/                 -- Fake Docker daemon (Docker API → containerd)
    artifacts/            -- OCI artifact extraction for macOS VM jobs
    metrics/              -- Prometheus metrics endpoint
    workflow/             -- Local workflow parser (ephemerd run)
    vm/                   -- Linux VM (WSL/Vz) and macOS VM (Vz)
  api/v1/                 -- gRPC protobuf definitions
  mage/                   -- Mage build and download targets
  magefile.go             -- Build system entry point (Mage, not Make)
  go.mod
  go.sum
```
