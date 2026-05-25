---
name: Ephemerd Engineer
description: Specialized agent for the ephemerd codebase — ephemeral GitHub Actions runner daemon (Go, Linux/Windows/macOS)
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

## Operations & Debug Runbook

Recipes for poking at a running ephemerd install — written for an agent that has just walked in and needs to see what's happening, fix it, redeploy, and move on. Prefer the `ephemerd` CLI subcommands over poking the underlying service manager directly; they wrap `sc.exe` / `launchctl` / `systemctl` and degrade gracefully.

### Day-1 inventory

```
ephemerd status              # is the service up? PID, uptime, active jobs
ephemerd jobs                # live job list (JOB ID / NAME / REPO / STATUS / UPTIME)
ephemerd logs -n 200         # last N lines from the service log
ephemerd logs -f             # follow live
ephemerd doctor              # platform sanity (Hyper-V/WSL/launchd state, stale dirs, sockets)
```

`ephemerd doctor` is the single best first-look command after any "is something broken?" question — it prints a checklist that catches stale PID files, orphan VM dirs, missing services, etc.

### Service control

Use the wrappers — they exist on all three platforms:

```
ephemerd start              # start the service
ephemerd stop               # stop (no drain)
ephemerd restart            # stop+start one-shot
ephemerd drain              # graceful: stop accepting new jobs, wait for in-flight
```

`ephemerd restart` runs `serviceAction("stop")` then `serviceAction("start")` — see `cmd/ephemerd/service.go`. If the wrapper can't reach the service (broken install, missing binary), fall back to:

- **Windows**: `sc.exe stop ephemerd` / `sc.exe start ephemerd` / `sc.exe query ephemerd`
- **Linux**: `systemctl stop|start|status ephemerd`
- **macOS**: `launchctl bootout|bootstrap system /Library/LaunchDaemons/com.ephpm.ephemerd.plist`

### Filesystem layout (operational)

| Path                                                  | Purpose                                            |
|-------------------------------------------------------|----------------------------------------------------|
| `C:\Program Files\ephemerd\ephemerd.exe`              | Windows: service-installed binary (what runs)      |
| `C:\Users\<user>\bin\ephemerd.exe`                    | Windows: convenience copy on PATH                  |
| `C:\ProgramData\ephemerd\ephemerd.exe`                | Windows: extra copy near data dir                  |
| `/usr/local/bin/ephemerd`                             | Linux/macOS: installed binary                      |
| `<DataDir>/config.toml`                               | Active config (TOML)                               |
| `<DataDir>/ephemerd.log`                              | Service log (text or JSON depending on config)     |
| `<DataDir>/ephemerd.pid`                              | PID file (removed on clean exit)                   |
| `<DataDir>/ephemerd.sock`                             | Control gRPC socket (used by `status`/`jobs`/etc.) |
| `<DataDir>/logs/<job>-runner.log`                     | Per-job runner log (preserved after destroy)       |
| `<DataDir>/jobs/<job>/docker/d.sock`                  | Per-job fake docker daemon socket                  |
| `<DataDir>/runners/job-<id>/`                         | Per-job runner dir (xcopy from `runners/<ver>/`)   |
| `<DataDir>/vm/linux/`                                 | Linux VM disk + console.log (Windows/macOS hosts)  |

`<DataDir>` is `C:\ProgramData\ephemerd` on Windows, `/var/lib/ephemerd` on Linux, `~/Library/Application Support/ephemerd` on macOS.

### Build & deploy on this host (Windows)

Two-stage Windows build embeds a Linux binary for the WSL/Hyper-V VM, then compiles the Windows host binary. From a feature worktree:

```
mage build:windows           # produces ./ephemerd.exe (~700 MB, embeds Linux binary)
ephemerd stop                # release the binary lock
cp ephemerd.exe "/c/Program Files/ephemerd/ephemerd.exe"
cp ephemerd.exe /c/Users/<user>/bin/ephemerd.exe
cp ephemerd.exe /c/ProgramData/ephemerd/ephemerd.exe   # optional
ephemerd start
ephemerd logs -n 50          # confirm version + clean startup
```

The version string in the startup log (`starting ephemerd version=...`) confirms the running binary matches the worktree commit.

### Auth: App vs PAT precedence

Code in `pkg/github/client.go`:

```go
if cfg.AppAuth != nil {
    // App: auto-refreshing installation token via custom http.RoundTripper
} else {
    // Static PAT fallback (cfg.Token)
}
```

`main.go` builds `AppAuth` whenever `cfg.GitHub.AppID != 0` and assigns it to `ghCfg.AppAuth`. **If `app_id`/`installation_id`/`private_key_path` are set in `config.toml`, the App wins and `GITHUB_TOKEN` is ignored entirely** — rotating `GITHUB_TOKEN` does *not* affect ephemerd polling in that case.

Auth-failure triage:
```
# Look for 401s in the log (all repos affected = App key/installation issue, not per-repo perms)
grep "401\|Bad credentials" <DataDir>/ephemerd.log | tail -20

# Test the App PEM + installation directly (Linux/macOS or git-bash)
gh auth status                                  # not authoritative for App
ls -la "<private_key_path>"                     # confirm PEM exists and mtime
```

If 401s span all repos, suspect: rotated App private key not deployed to `private_key_path`; clock skew; GitHub-side outage. If they're per-repo, suspect: the App installation lost access to that repo.

### Local CI compromise (Windows hosts only)

`mage ci` / `mage lint` trips a known cgo failure on Windows: `miekg/pkcs11` (transitively via `containers/ocicrypt`) can't be preprocessed by the Windows cgo toolchain. This is documented in `AGENTS.md` as a *local* problem, not a CI problem.

Workaround that gives the same coverage as remote CI without leaving Windows:

```
GOOS=linux ./bin/golangci-lint.exe run ./...        # full lint, GOOS-cross
GOOS=linux go test -count=1 -run xxx ./pkg/...      # compile-only check
                                                    # (exit "fork/exec: not a valid Win32 app" = compile OK)
go test -count=1 ./pkg/config/... ./pkg/runtime/... # natively-runnable packages
```

The compile-only `-run xxx` trick is what AGENTS.md endorses for the cgo-affected packages (`pkg/containerd`, `pkg/dind`, `pkg/workflow`, `cmd/ephemerd`).

### Job lifecycle in the log

A successful job leaves this trail (Linux dispatched via WSL):
```
"provisioning Linux runner via dispatch" job_id=<X> dispatch=linux
"using image for job"                    job_id=<X> image=<I>
"registered repo-level JIT runner"       name=<R-runner-name>
"Linux runner dispatched"                job_id=<X> name=<R>
"dispatched runner exited"               job_id=<X> exit_code=0
```

Windows native job:
```
"provisioning runner for job"            job_id=<X>
"runner environment ready"               job_id=<X> name=<R>
"runner exited"                          job_id=<X> exit_code=0
"runner environment destroyed"           id=<R>
```

Trace one job: `awk '/<job_id>/' <DataDir>/ephemerd.log`. Per-job runner log preserved at `<DataDir>/logs/<runner-name>-runner.log` even after destroy.

### Worktree + commit conventions

User maintains hard rules captured in memory; the short version:

- **Always work in a per-feature worktree** under `.claude/worktrees/<feature>` (`git worktree add .claude/worktrees/<feature> -b <branch> origin/main`). Never edit the main worktree for branch work.
- **Backdate commits to the prior evening** when the user approves a commit (`GIT_AUTHOR_DATE`/`GIT_COMMITTER_DATE` to ~20:00–23:00 local previous day).
- **No `_ =` to silence errors** — wrap fallible calls in `if err := …; err != nil { log.Warn(…) }`.
- **Use the user's `GITHUB_TOKEN`** for `git push` / `gh` — never the GitHub App bot. Don't add Claude attribution to commits.

### CI matrix gotchas

`Build (Linux arm64)` and `Build (macOS arm64)` jobs in `.github/workflows/ci.yml` are gated behind repo variables (`HAS_LINUX_ARM64_RUNNER`, `HAS_MACOS_ARM64_RUNNER`) and `continue-on-error: true`. With the vars unset they show **Skipped**, not Pending — see PR #75. To enable once runners are live: `gh variable set HAS_LINUX_ARM64_RUNNER --body true`.

`Build (Windows amd64)` is unconditional and runs on this host. If you redeploy mid-build, the running CI job dies — re-run the failed check after the deploy.

## Current Branch: feat/windows-support

PR #9: https://github.com/ephpm/ephemerd/pull/9

Windows support with single-poller dispatch architecture. Windows host runs native Hyper-V containers for Windows jobs and dispatches Linux jobs to a WSL2 worker via gRPC.
