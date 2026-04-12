# Contributing to ephemerd

## Prerequisites

- **Go 1.23+**
- **[Mage](https://magefile.org/)** — `go install github.com/magefile/mage@latest`
- **Linux, macOS (Apple Silicon), or Windows** — ephemerd builds on all three

## Dev setup

```bash
git clone https://github.com/ephpm/ephemerd.git
cd ephemerd

# Download all embedded dependencies (runner binary, CNI plugins, containerd shim, Alpine rootfs)
mage download:all

# Build for the current platform
mage build

# Run linter + tests + build (same as CI)
mage ci
```

### Individual targets

```bash
mage lint          # download golangci-lint and run it
mage test          # download deps and run all tests
mage build:build   # compile for current OS
mage build:windows # two-stage Windows build (embeds Linux binary for WSL)
mage e2E           # unprivileged e2e tests (needs GITHUB_TOKEN)
mage e2EAll        # all e2e tests (needs root + containerd)
```

Run `mage -l` for the full list.

## Project layout

```
cmd/ephemerd/       CLI entry point (urfave-cli/v3)
pkg/                library packages
  config/           TOML config parsing
  containerd/       in-process containerd server
  dind/             fake Docker daemon (not yet merged)
  github/           GitHub API client + webhook handler
  networking/       CNI (Linux), HCN (Windows), passthrough (macOS)
  runtime/          container lifecycle (create/wait/destroy)
  scheduler/        job discovery, routing, dispatch
  vm/               Linux VM (WSL/Vz) and macOS VM (Vz)
api/v1/             gRPC protobuf definitions
mage/               Mage build and download targets
docs/arch/          architecture decision docs
examples/           deployment examples (Terraform)
test/e2e/           end-to-end tests
```

## Running tests

```bash
# Unit tests (no special requirements)
mage test

# Or directly:
go test ./pkg/...

# E2E tests (needs GITHUB_TOKEN for webhook round-trip)
GITHUB_TOKEN=ghp_... mage e2E

# Privileged e2e (needs root, runs containerd)
sudo go test -tags "e2e,privileged" -v -timeout 5m ./test/e2e/...
```

The `pkg/vm` tests on Windows require a dummy `pkg/vm/embed/ephemerd-linux` file (created by `mage download:all` or `mage build`).

## Code style

- **No `_ =` to silence errors.** Always handle errors — check and log, return, or wrap. Add a comment explaining why only if there is truly no way to handle it (e.g., deferred Close with no logger).
- **No Co-Authored-By lines** in commits.
- Run `mage ci` before pushing — it runs the same lint + test + build pipeline as GitHub Actions.

## Platform-specific files

Platform code uses build tags:

- `*_linux.go` — Linux-only (CNI, iptables, seccomp)
- `*_windows.go` — Windows-only (HCN, Hyper-V, WSL)
- `*_darwin.go` — macOS-only (Virtualization.framework)
- `*_stub.go` / `*_other.go` — fallback stubs for other platforms

## Architecture docs

Design decisions and future plans are documented in `docs/arch/`:

- `overview.md` — full system architecture
- `macos.md` — macOS Linux VM + per-job macOS VMs
- `windows-single-scheduler.md` — Windows WSL dispatch model
- `dind.md` — fake Docker daemon design (not yet implemented)
- `gitlab.md` — GitLab integration design (not yet implemented)
- `webhooks.md` — webhook tunnel architecture
- `rootfs.md` — pre-baked Alpine rootfs for Linux VMs
