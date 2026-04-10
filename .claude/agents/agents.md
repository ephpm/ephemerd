---
name: ephemerd-engineer
---

# Ephemerd

Ephemeral GitHub Actions runner daemon. Single Go binary that manages isolated, disposable CI/CD environments — each job gets a fresh environment destroyed on completion.

## Architecture

- **Linux**: Direct OCI containers via embedded containerd (in-process library, not external binary)
- **Windows**: Hyper-V isolated containers via embedded containerd + WSL2 Linux VM for Linux jobs
- **macOS**: Virtualization.framework Linux VM + copy-on-write macOS VM snapshots

### Single-Poller Dispatch (Windows + WSL)

One scheduler on Windows dispatches Linux jobs to WSL via gRPC. WSL runs containerd-only (no scheduler, no GitHub credentials).

```
Windows Host (ephemerd.exe serve):
  ├─ Containerd (Windows, named pipe)
  ├─ Scheduler (single poller for ALL jobs)
  │   ├─ Windows job → local Runtime.Create() on Windows containerd
  │   └─ Linux job → gRPC DispatchClient → WSL dispatch server
  └─ WSL VM boot (containerd-only + dispatch worker)

WSL (ephemerd serve --containerd-only):
  ├─ Containerd (Linux, TCP :10000)
  ├─ Pre-built rootfs with runner, CNI, gcompat, iptables baked in
  ├─ Networking (CNI bridge, stale bridge cleanup on startup)
  └─ Dispatch gRPC server (TCP :10001)
      ├─ CreateJob → local Runtime.Create()
      ├─ WaitJob  → local Runtime.Wait()
      └─ DestroyJob → local Runtime.Destroy()
```

See `docs/arch/windows-single-scheduler.md` for full design.

## Module & Layout

Module: `github.com/ephpm/ephemerd` | Go 1.24.3

```
api/v1/                 gRPC control + dispatch API (protobuf)
  ephemerd.proto        Control service + Dispatch service (CreateJob/WaitJob/DestroyJob)

cmd/ephemerd/           CLI entry point (urfave/cli v3)
  main.go               Commands: serve, status, drain, images, config, ctrctl
  commands.go           Command implementations
  run.go                Run command (single-shot job execution)
  runtime_default.go    Linux runtime startup
  runtime_darwin.go     macOS runtime startup (Virtualization.framework)
  runtime_windows.go    Windows runtime startup (containerd + WSL2 VM + dispatch client)

pkg/cni/                Embedded CNI plugin binaries (go:embed, Linux only)
pkg/config/             TOML config parsing (BurntSushi/toml)
pkg/containerd/         Embedded containerd server (containerd/v2 as library)
  server.go             In-process containerd, optional TCP listener (--containerd-tcp-port)
  listen_unix.go        Unix socket listener
  listen_windows.go     Named pipe listener (go-winio)
  shim_linux.go         Extracts embedded shim/runc to <datadir>/bin/
  shim_other.go         No-op on non-Linux (Windows uses Hyper-V isolation)
pkg/github/             GitHub API client (go-github/v72), JIT runner registration
pkg/networking/         Platform-specific networking
  networking.go         Manager, CleanStaleBridge(), subnet auto-selection
  network_linux.go      CNI bridge networking (ephemerd0 bridge)
  network_windows.go    HCN NAT networking with per-endpoint ACL policies
  network_darwin.go     Passthrough (delegates to Linux VM)
pkg/runtime/            Container lifecycle: Create/Destroy/Wait/PullImage
  runtime.go            Main lifecycle, platform-specific paths for Windows
  image_default.go      Linux default: ghcr.io/actions/actions-runner:latest
  image_windows.go      Auto-detects Windows build → servercore:ltsc20XX
pkg/runner/             Embedded GitHub Actions runner binary (go:embed)
  runner.go             Extracts runner, handles .tar.gz (Linux) and .zip (Windows)
pkg/scheduler/          Job orchestration: polling/webhooks, concurrency, health endpoint
  scheduler.go          Job discovery, routes Linux jobs to WSL dispatcher
  dispatch.go           gRPC dispatch server (WSL) + client (Windows)
  grpc.go               gRPC service for Control API (status, jobs, logs)
pkg/vm/                 VM management for macOS/Windows
  vm.go                 LinuxVM interface (Client, DispatchAddr, Stop)
  linuxvm_windows.go    WSL2 Linux VM (distro import, /mnt/c/ binary exec, dispatch)
  linuxvm_darwin.go     Virtualization.framework Linux VM
  linuxvm_linux.go      No-op stub (Linux runs containerd natively)
  wslrun_windows.go     WSL command execution helper (UTF-16LE output decoding)
  embed_windows.go      go:embed for Linux ephemerd binary + pre-built rootfs
  macosvm_darwin.go     macOS VM via Virtualization.framework
pkg/workflow/           Job label routing (platform detection from runs-on labels)
  platform.go           Detects target OS from job labels (linux/windows/macos)

docs/arch/              Architecture documentation
  overview.md           System overview
  windows-single-scheduler.md   Single-poller dispatch design
  rootfs.md             Pre-built WSL rootfs process
  macos.md              macOS VM architecture
```

## Build & Test

Build system uses [Mage](https://magefile.org/):

```bash
mage build              # Linux: downloads assets + builds binary
mage build:windows      # Two-stage: cross-compile Linux binary, embed in Windows build
mage build:linuxForEmbed # Cross-compile static Linux ephemerd for embedding
mage generate           # Regenerate protobuf Go code
mage test               # go test ./...
mage lint               # golangci-lint run ./...
mage clean              # Remove binaries + embedded artifacts
```

### Windows Two-Stage Build

`mage build:windows` does:
1. Download Linux assets (runner tarball, CNI, shim, runc)
2. Cross-compile static Linux ephemerd (`CGO_ENABLED=0 GOOS=linux`)
3. Pre-built rootfs with gcompat + iptables baked in (see `docs/arch/rootfs.md`)
4. Swap in Windows runner zip
5. Build Windows binary (embeds Linux binary + rootfs + Windows runner)

The Windows binary is ~550MB because it embeds everything for both platforms.

### Cross-compile binary location

Windows binaries go to `/mnt/c/Users/luthe/bin/ephemerd.exe` (Luther's Windows desktop).

Version injected via ldflags: `main.version` and `pkg/runner.Version`.

## Key Types

- `config.Config` — top-level config (GitHub, Containerd, VM, Runner, Log sections)
- `runtime.Runtime` — container lifecycle (holds containerd `client.Client`)
- `runtime.RunnerEnv` — a running job environment (container + task + netns)
- `scheduler.Scheduler` — job discovery, concurrency semaphore, health endpoint
- `scheduler.DispatchClient` — gRPC client for dispatching Linux jobs to WSL
- `github.Client` — GitHub API, JIT runner registration/deregistration
- `networking.Manager` — platform-specific network setup + firewall

## Job Lifecycle

### Local Jobs (Linux-on-Linux, Windows-on-Windows)

1. Discover job (poll GitHub API or receive webhook)
2. Register JIT runner via GitHub API
3. Create container (pull image, setup networking, mount runner)
4. Wait for task exit
5. Destroy container, teardown networking, deregister runner

### Dispatched Linux Jobs (Windows host → WSL)

1. Windows scheduler discovers job with `linux` label
2. Register JIT runner with `["self-hosted", "linux", "x64"]` labels
3. `DispatchClient.Create(id, image, jitConfig)` → gRPC to WSL dispatch server
4. WSL server creates Linux container using its local Runtime
5. `DispatchClient.Wait(id)` → blocks until job completes
6. `DispatchClient.Destroy(id)` → cleans up container + networking in WSL

## Windows-Specific Details

### Container Networking
- Uses HCN (Host Compute Network) NAT network, not CNI
- HCN endpoints created BEFORE the container (unlike Linux CNI which attaches after)
- Endpoint ID added to OCI spec's `Windows.Network.EndpointList`
- HCN namespace created and endpoint attached for Hyper-V isolated containers
- Per-endpoint ACL policies block RFC1918 + link-local ranges

### Container I/O
- `cio.NullIO` used (file:// and stdio both fail with runhcs)
- Runner output captured via cmd.exe redirect to `C:\actions-runner\runner.log`

### Runner Mount
- Always mounted on Windows (no pre-built Windows GHA runner image exists)
- Per-job copies made with `xcopy` (not `cp -al` like Linux)
- Entrypoint wraps run.cmd: `cmd.exe /c run.cmd --jitconfig ... > runner.log 2>&1`

### WSL2 Linux VM (vm.linux.enabled = true)
- Boots async in background goroutine (doesn't block Windows jobs)
- Creates WSL distro from embedded pre-built rootfs (gcompat + iptables pre-installed)
- Runs Linux ephemerd binary from `/mnt/c/` (Windows disk mount, avoids slow 9P copy)
- Launches with `--containerd-only` flag (containerd + dispatch gRPC server, no scheduler)
- No GitHub credentials needed in WSL (single poller on Windows handles all auth)
- Windows host connects to WSL dispatch server via TCP gRPC on containerdPort + 1
- Stale CNI bridge cleaned on worker startup (all WSL2 distros share one Linux kernel)
- Distro destroyed on shutdown (`wsl --unregister`)

### Snapshotter
- Linux: overlayfs
- Windows: windows (NOT overlayfs)

## Conventions

- Logging: stdlib `log/slog` (structured, text or JSON)
- Errors: wrap with context `fmt.Errorf("operation: %w", err)` — never suppress with `_ =`
- Platform code: build tags + `_linux.go`, `_darwin.go`, `_windows.go` suffixes
- Default image auto-detected per platform (configurable via `runner.default_image`)
- Concurrency: semaphore channel pattern, context-based cancellation throughout
- Config: TOML at `$EPHEMERD_DATA_DIR/config.toml` (default `/var/lib/ephemerd` or `C:\ProgramData\ephemerd`)
- Security: containers blocked from RFC1918 + link-local ranges, internet allowed
- embed.FS paths: always use forward slashes (even on Windows)

## CI

GitHub Actions workflows on self-hosted runners:
- `ci.yml`: go vet → golangci-lint → go test → build
- `test-runner.yml`: E2E smoke tests — dispatches both Linux and Windows jobs (manual dispatch)
- `test-windows.yml`: Windows smoke tests (manual dispatch, `runs-on: [self-hosted, windows, x64]`)

## Current Branch: feat/windows-support

PR #9: https://github.com/ephpm/ephemerd/pull/9

Windows support with single-poller dispatch architecture. Windows host runs native Hyper-V containers for Windows jobs and dispatches Linux jobs to a WSL2 worker via gRPC.
