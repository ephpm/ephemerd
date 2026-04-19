---
title: Project Layout
weight: 2
---

## Directory structure

```
cmd/ephemerd/           CLI entry point (urfave-cli/v3)
pkg/                    Library packages
  config/               TOML config parsing
  containerd/           In-process containerd server
  dind/                 Fake Docker daemon for container-in-container workflows
  github/               GitHub API client + webhook handler
  networking/           CNI (Linux), HCN (Windows), passthrough (macOS)
  runtime/              Container lifecycle (create/wait/destroy)
  scheduler/            Job discovery, routing, dispatch
  tunnel/               Webhook tunnel providers (localtunnel, ngrok)
  providers/            Multi-forge provider interface (GitHub, Forgejo, Gitea, GitLab, Woodpecker)
  metrics/              Prometheus metrics endpoint
  artifacts/            OCI artifact extraction for macOS VM jobs
  workflow/             Local workflow parser and runner (ephemerd run)
  vm/                   Linux VM (WSL/Vz) and macOS VM (Vz)
  runner/               Embedded GitHub Actions runner binary
api/v1/                 gRPC protobuf definitions
mage/                   Mage build and download targets
docs/                   Documentation (this site)
examples/               Deployment examples (Terraform)
test/e2e/               End-to-end tests
```

## Platform-specific files

Platform code uses Go build tags and file name suffixes:

| Suffix | Platform | Examples |
|---|---|---|
| `*_linux.go` | Linux only | CNI networking, iptables, seccomp |
| `*_windows.go` | Windows only | HCN networking, Hyper-V, WSL |
| `*_darwin.go` | macOS only | Virtualization.framework |
| `*_stub.go` / `*_other.go` | Fallback stubs | No-op implementations for unsupported platforms |

Most packages in `pkg/` contain platform-specific files alongside shared code. For example, `pkg/networking/` has `network_linux.go`, `network_windows.go`, and `network_darwin.go` implementing the same `Manager` interface for each OS.

## Key types

| Type | Package | Purpose |
|---|---|---|
| `Config` | `pkg/config` | Top-level config (GitHub, Containerd, VM, Runner, Log sections) |
| `Runtime` | `pkg/runtime` | Container lifecycle (holds containerd client) |
| `RunnerEnv` | `pkg/runtime` | A running job environment (container + task + netns) |
| `Scheduler` | `pkg/scheduler` | Job discovery, concurrency semaphore, health endpoint |
| `Client` | `pkg/github` | GitHub API, JIT runner registration/deregistration |
| `Manager` | `pkg/networking` | Platform-specific network setup + firewall |
