# Ephemerd

Ephemeral GitHub Actions runner daemon. Single Go binary that manages isolated, disposable CI/CD environments — each job gets a fresh environment destroyed on completion.

## Architecture

- **Linux**: Direct OCI containers via embedded containerd (in-process library, not external binary)
- **Windows**: Hyper-V isolated containers + optional Linux VM for Linux jobs
- **macOS**: Virtualization.framework Linux VM + copy-on-write macOS VM snapshots

## Module & Layout

Module: `github.com/ephpm/ephemerd` | Go 1.24.3

```
cmd/ephemerd/           CLI entry point (urfave/cli v3)
  main.go               Commands: serve, status, drain, images, config, ctrctl
  commands.go           Command implementations
  runtime_default.go    Linux/Windows runtime startup
  runtime_darwin.go     macOS runtime startup (Virtualization.framework)
  runtime_windows.go    Windows-specific containerd setup

pkg/config/             TOML config parsing (BurntSushi/toml)
pkg/containerd/         Embedded containerd server (containerd/v2 as library)
pkg/github/             GitHub API client (go-github/v72), JIT runner registration
pkg/networking/         Platform-specific CNI/HCN/VM networking + firewall rules
pkg/runtime/            Container lifecycle: Create/Destroy/Wait/PullImage
pkg/runner/             Embedded GitHub Actions runner binary (go:embed)
pkg/scheduler/          Job orchestration: polling/webhooks, concurrency, health endpoint
pkg/vm/                 VM management for macOS/Windows
```

## Build & Test

```bash
make build          # downloads runner tarball + builds binary
make test           # go test ./...
make lint           # golangci-lint run ./...
make download-runner # just the runner tarball
make clean          # remove binary + runner artifacts
```

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
3. Create container (pull image, setup networking, mount runner read-only)
4. Wait for task exit
5. Destroy container, teardown networking, deregister runner

## Conventions

- Logging: stdlib `log/slog` (structured, text or JSON)
- Errors: wrap with context `fmt.Errorf("operation: %w", err)` — never suppress with `_ =`
- Platform code: build tags + `_linux.go`, `_darwin.go`, `_windows.go` suffixes
- No default image: jobs must specify their image via `EPHEMERD_IMAGE` — no fallback
- Concurrency: semaphore channel pattern, context-based cancellation throughout
- Config: TOML at `$EPHEMERD_DATA_DIR/config.toml` (default `/var/lib/ephemerd`)
- Security: containers blocked from RFC1918 + link-local ranges, internet allowed

## CI

Single GitHub Actions workflow (`ci.yml`) on self-hosted Linux runners:
go vet → golangci-lint → go test -race → build

E2E security tests (`test-runner.yml`, manual dispatch): LAN isolation, container escape, namespace isolation, read-only mounts.

Release via goreleaser: linux/{amd64,arm64}, windows/amd64, darwin/{amd64,arm64}.
