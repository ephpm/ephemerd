---
name: ephemerd-engineer
---

# Ephemerd

Ephemeral GitHub Actions runner daemon. Single Go binary that manages isolated, disposable CI/CD environments — each job gets a fresh environment destroyed on completion.

## Architecture

- **Linux**: Direct OCI containers via embedded containerd (in-process library, not external binary)
- **Windows**: Hyper-V isolated containers via embedded containerd + WSL2 Linux VM for Linux jobs
- **macOS**: Virtualization.framework Linux VM + copy-on-write macOS VM snapshots

## Module & Layout

Module: `github.com/ephpm/ephemerd` | Go 1.24.3

```
cmd/ephemerd/           CLI entry point (urfave/cli v3)
  main.go               Commands: serve, status, drain, images, config, ctrctl
  commands.go           Command implementations
  runtime_default.go    Linux runtime startup
  runtime_darwin.go     macOS runtime startup (Virtualization.framework)
  runtime_windows.go    Windows runtime startup (containerd + WSL2 Linux VM)

pkg/config/             TOML config parsing (BurntSushi/toml)
pkg/containerd/         Embedded containerd server (containerd/v2 as library)
  server.go             In-process containerd, optional TCP listener (--containerd-tcp-port)
  listen_unix.go        Unix socket listener
  listen_windows.go     Named pipe listener (go-winio)
  shim_linux.go         Extracts embedded shim/runc to <datadir>/bin/
  shim_other.go         No-op on non-Linux (Windows uses Hyper-V isolation)
pkg/github/             GitHub API client (go-github/v72), JIT runner registration
pkg/networking/         Platform-specific networking
  network_linux.go      CNI bridge networking
  network_windows.go    HCN NAT networking with per-endpoint ACL policies
  network_darwin.go     Passthrough (delegates to Linux VM)
pkg/runtime/            Container lifecycle: Create/Destroy/Wait/PullImage
  runtime.go            Main lifecycle, platform-specific paths for Windows
  image_default.go      Linux default: ghcr.io/actions/actions-runner:latest
  image_windows.go      Auto-detects Windows build → servercore:ltsc20XX
pkg/runner/             Embedded GitHub Actions runner binary (go:embed)
  runner.go             Extracts runner, handles .tar.gz (Linux) and .zip (Windows)
pkg/scheduler/          Job orchestration: polling/webhooks, concurrency, health endpoint
pkg/vm/                 VM management for macOS/Windows
  linuxvm_windows.go    WSL2-based Linux VM (imports distro, embeds ephemerd binary)
  linuxvm_darwin.go     Virtualization.framework Linux VM
  embed_windows.go      go:embed for Linux ephemerd binary + Alpine rootfs
```

## Build & Test

```bash
make build              # Linux: downloads assets + builds binary
make build-windows      # Two-stage: cross-compile Linux binary, embed in Windows build
make test               # go test ./...
make lint               # golangci-lint run ./...
make clean              # remove binaries + embedded artifacts
```

### Windows Two-Stage Build

`make build-windows` does:
1. Download Linux assets (runner tarball, CNI, shim, runc)
2. Cross-compile static Linux ephemerd (`CGO_ENABLED=0 GOOS=linux`)
3. Download Alpine minirootfs for WSL
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
- `github.Client` — GitHub API, JIT runner registration/deregistration
- `networking.Manager` — platform-specific network setup + firewall

## Job Lifecycle

1. Discover job (poll GitHub API or receive webhook)
2. Register JIT runner via GitHub API
3. Create container (pull image, setup networking, mount runner)
4. Wait for task exit
5. Destroy container, teardown networking, deregister runner

## Windows-Specific Details

### Container Networking
- Uses HCN (Host Compute Network) NAT network, not CNI
- HCN endpoints are created BEFORE the container (unlike Linux CNI which attaches after)
- Endpoint ID is added to OCI spec's `Windows.Network.EndpointList`
- runhcs reads the endpoint list and attaches network during container creation

### Container I/O
- `cio.LogFile` (file:// URI) rejected by runhcs — requires binary:// scheme
- `cio.WithStdio` fails with Access Denied on named pipes
- Currently uses `cio.NullIO` — runner output captured via cmd.exe redirect to runner.log

### Runner Mount
- Always mounted on Windows (no pre-built Windows GHA runner image exists)
- Per-job copies made with `xcopy` (not `cp -al` like Linux)
- Entrypoint wraps run.cmd: `cmd.exe /c run.cmd --jitconfig ... > runner.log 2>&1`

### WSL2 Linux VM (vm.linux.enabled = true)
- Boots async in background goroutine (doesn't block Windows jobs)
- Creates WSL distro from embedded Alpine rootfs
- Copies embedded Linux ephemerd binary via `\\wsl$\ephemerd-linux\...` UNC path
- Runs with `--containerd-only` flag (just containerd, no scheduler)
- Copies host config.toml into distro for GitHub credentials
- Windows host connects to WSL containerd via TCP (gRPC, bypassing containerd's npipe dialer)
- Distro destroyed on shutdown (`wsl --unregister`)

### Known Issues (in progress)
- Container networking: HCN endpoints attached to OCI spec but DNS may not work yet
- containerd logrus output renders poorly in PowerShell (missing \r in line endings)
- Snapshot cleanup sometimes fails with Access Denied on shutdown

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
- `test-runner.yml`: Linux E2E smoke tests (manual dispatch)
- `test-windows.yml`: Windows smoke tests (manual dispatch, `runs-on: [self-hosted, windows, x64]`)

## Current Branch: feat/windows-support

PR #9: https://github.com/ephpm/ephemerd/pull/9

Active work on Windows support. The Windows desktop runs ephemerd natively (admin required) alongside a WSL Linux instance. Both poll the same repos. Runner names include OS prefix to avoid collisions.
