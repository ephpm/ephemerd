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
/var/lib/ephemerd/              # Linux
C:\ProgramData\ephemerd\        # Windows
  containerd/
    state/                      # containerd runtime state
    root/                       # image store, snapshots
  runners/                      # ephemeral runner workdirs (cleaned per job)
  config.toml                   # ephemerd config
```

### Isolation Backends

ephemerd abstracts over platform-specific isolation mechanisms through a common `Backend` interface:

#### Linux: containerd (in-process)

- Standard OCI containers via embedded containerd
- Optional Firecracker microVM backend for stronger isolation
- Supports x86_64 and aarch64
- Fast startup (~1s for containers, ~125ms for Firecracker)
- Pre-built images with build tools (gcc, cmake, etc.)

#### Windows: containerd + Hyper-V Isolation (in-process)

- containerd runs natively on Windows and supports Hyper-V isolation
- Each container gets its own kernel in a lightweight VM
- Real isolation -- malicious code cannot escape to the host
- Same embedded containerd approach, just compiled for Windows
- Supports x86_64 (aarch64 when ecosystem matures)
- Pre-built images based on `mcr.microsoft.com/windows/servercore` with MSVC build tools
- Startup ~5-10s (acceptable for CI jobs running 20+ minutes)

#### macOS: Future

- Tart (Virtualization.framework) for Apple Silicon VMs
- Apple licensing restricts VMs to Apple hardware
- Lower priority -- defer until Linux + Windows are solid

### Build Matrix

Each OS/arch combination produces one self-contained binary with containerd compiled in:

| Target | Binary | Isolation |
|--------|--------|-----------|
| linux/amd64 | `ephemerd` | OCI containers / Firecracker |
| linux/arm64 | `ephemerd` | OCI containers / Firecracker |
| windows/amd64 | `ephemerd.exe` | Hyper-V containers |
| windows/arm64 | `ephemerd.exe` | Hyper-V containers |

No runtime dependencies beyond the OS kernel and (on Windows) Hyper-V.

### GitHub Integration

Two options for receiving jobs:

**Option A: Runner Scale Set Client**
- GitHub's newer Go module for custom runner orchestration
- More control over provisioning lifecycle
- Replaces the older runner registration model
- Natural fit since it's already Go

**Option B: Webhook + JIT Runners**
- Listen for `workflow_job` webhook events
- Register a just-in-time (JIT) runner for each job
- Simpler to implement, well-documented

Start with Option B for simplicity, migrate to Option A if needed.

### Image Management

Pre-built base images per platform with common build tools:

**Linux images:**
- `ephemerd/linux-build` -- gcc, cmake, autoconf, make, pkg-config, Rust toolchain
- `ephemerd/linux-php` -- above + PHP build dependencies for spc

**Windows images:**
- `ephemerd/windows-build` -- MSVC Build Tools, cmake, git, 7-zip
- `ephemerd/windows-php` -- above + PHP SDK binary tools, spc dependencies

Images are pulled/cached by the embedded containerd. ephemerd manages the cache and pulls updates on a configurable schedule.

### Configuration

Single TOML config file:

```toml
[github]
# App installation or PAT for runner registration
app_id = 12345
private_key_path = "/etc/ephemerd/github-app.pem"
# Or: token = "ghp_..."

# Which org/repo to register runners for
owner = "ephpm"
repos = ["ephpm", "php-sdk", "litewire"]

[containerd]
# Data directory (defaults to /var/lib/ephemerd or C:\ProgramData\ephemerd)
root = "/var/lib/ephemerd/containerd"
# Snapshotter (overlayfs on Linux, windows on Windows)
# snapshotter = "overlayfs"

[isolation]
# Linux: "container" (default) or "firecracker"
type = "container"
# Windows: always Hyper-V, no configuration needed

[runner]
# Labels applied to all runners (in addition to OS/arch labels)
extra_labels = ["self-hosted"]
# Max concurrent jobs
max_concurrent = 4
# Default container image
default_image = "ghcr.io/ephpm/ephemerd-build:latest"
# Timeout for jobs before forced teardown
job_timeout = "2h"
```

### Runner Labels

ephemerd auto-applies labels based on the environment:

- `self-hosted`
- OS: `linux` or `windows`
- Arch: `x64` or `arm64`
- Custom labels from config

Workflows select runners as usual:

```yaml
runs-on: [self-hosted, linux, x64]    # Linux container
runs-on: [self-hosted, windows, x64]  # Hyper-V container
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

This is functionally identical -- `services:` is syntactic sugar. Document this for users and move on.

### macOS

Not in initial scope. Requires Apple hardware and Virtualization.framework. Add when Linux + Windows are stable.

### ARM64 Windows

PHP and its toolchain don't support Windows ARM64 yet. ephemerd can support it at the infrastructure level (Hyper-V containers work on ARM64 Windows), but there's nothing useful to run in them until the ecosystem catches up.

## Tech Stack

- **Language:** Go
- **containerd:** imported as library (github.com/containerd/containerd/v2), runs in-process
- **GitHub API:** go-github or runner scale set client module
- **Config:** TOML (BurntSushi/toml or pelletier/go-toml)
- **Logging:** slog (stdlib structured logging)
- **CLI:** cobra or urfave/cli

## Project Structure

```
ephemerd/
  cmd/
    ephemerd/
      main.go             -- CLI entry point, config loading, daemon lifecycle
  pkg/
    config/
      config.go           -- Configuration structs, TOML parsing
    github/
      client.go           -- GitHub API client, webhook handling
      runner.go           -- Runner registration/deregistration, JIT tokens
    runtime/
      runtime.go          -- Backend interface definition
      containerd.go       -- Embedded containerd setup, container lifecycle
      hyperv.go           -- Windows Hyper-V isolation options
      firecracker.go      -- Linux Firecracker microVM backend (future)
    scheduler/
      scheduler.go        -- Job queue, concurrency limits, environment lifecycle
    image/
      image.go            -- Image pulling, caching, updates via containerd
  Dockerfile              -- For building ephemerd itself
  Makefile
  go.mod
  go.sum
```
