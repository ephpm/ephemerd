---
title: System Overview
weight: 1
---

High-level architecture of ephemerd: a cross-platform daemon for managing ephemeral GitHub Actions self-hosted runners.

## Problem

No existing solution manages ephemeral CI runners across Linux, Windows, and macOS from a single control plane:

- **ARC (Actions Runner Controller)** -- Kubernetes-only, Linux-only, no Windows support.
- **Firecracker-based** (fireactions, appsignal) -- Linux-only microVMs.
- **GitHub hosted runners** -- no ARM64 Windows, limited ARM64 Linux, expensive macOS, no environment control.
- **Community Windows containers** -- manual Docker setups, no orchestration, no real isolation.

Self-hosted runners on bare metal are insecure for public repos -- any PR can run arbitrary code on the host. ephemerd solves this by running every job in an ephemeral, isolated environment that is destroyed after the job completes.

## Why Go

The entire ecosystem ephemerd integrates with is Go:

- **containerd** -- Go library, designed to be imported directly (k3s/rke2 proved this).
- **GitHub Actions runner scale set client** -- Go module.
- **OCI/container specs** -- Go reference implementations.

By writing ephemerd in Go, containerd runs in-process as a library -- no binary embedding, no child process management, no extract-and-spawn lifecycle. Direct access to containerd's internal APIs for snapshots, tasks, namespaces, and image management. One binary, one process, no version mismatches.

## Core Loop

1. Register with the forge as a runner (or poll for jobs via webhook).
2. Receive job assignment.
3. Provision an ephemeral environment (container or VM) from a pre-built image.
4. Install and start the runner binary inside the environment.
5. Job executes in full isolation from the host.
6. On completion (success or failure), tear down the environment -- clean slate.

## Embedded containerd

Following the k3s/rke2 model, ephemerd imports containerd as a Go library and runs it in-process. No external containerd install, no system service, no socket management, no version mismatches.

How it works:

- Import `github.com/containerd/containerd/v2` packages directly.
- Start containerd's server components in a goroutine within the ephemerd process.
- containerd's gRPC services are available in-process (a socket is also exposed for debugging tools).
- Snapshotter, content store, and image service all run in the same process.
- On shutdown, containerd tears down cleanly with the parent process.

What this gives us:

- Single binary deployment -- `ephemerd` is all you install.
- No separate containerd service to configure, upgrade, or monitor.
- No socket permissions issues.
- Direct Go API access instead of gRPC round-trips for internal operations.
- Consistent containerd version across all deployments.

Data directory layout:

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

See [Embedded containerd]({{< relref "embedded-containerd" >}}) for a deep dive.

## Isolation Model

containerd manages OCI images and container lifecycle on every platform. The isolation mechanism differs by host OS, but the image format is always OCI -- one Dockerfile builds images that run everywhere.

### Linux: containerd containers (direct)

Standard OCI containers via embedded containerd, running directly on the host kernel. Supports x86_64 and aarch64. Fast startup (~1s). Networking via CNI bridge with iptables rules blocking RFC1918 ranges.

### Windows: containerd + Hyper-V isolation

containerd runs natively on Windows and supports Hyper-V isolation. Each container gets its own kernel in a lightweight VM -- real isolation, malicious code cannot escape to the host. Same OCI images, same containerd APIs, just compiled for Windows. Startup ~5-10s. Networking via HCN (Host Compute Network) with NAT and per-endpoint ACL policies.

Linux jobs on a Windows host are dispatched to a WSL2 worker via gRPC. See [Windows WSL dispatch]({{< relref "windows-wsl-dispatch" >}}).

### macOS: Virtualization.framework

macOS cannot run OCI containers natively. ephemerd boots a lightweight Linux VM using Apple's Virtualization.framework (built into macOS 12+, no third-party deps). containerd runs inside the Linux VM for Linux jobs. macOS-native jobs (Xcode, Swift) get per-job ephemeral macOS VMs via APFS clone-on-write.

See [macOS VMs]({{< relref "macos-vms" >}}).

## Dual-Purpose Hosts

Because Windows can run Hyper-V Linux VMs and macOS can run Virtualization.framework Linux VMs, a single machine can serve multiple job types:

| Host | Linux jobs | Native OS jobs |
|------|-----------|----------------|
| Linux x86_64 | containerd (direct) | -- |
| Linux arm64 | containerd (direct) | -- |
| Windows x86_64 | containerd in WSL2 Linux VM | Hyper-V Windows containers |
| macOS arm64 | containerd in Virtualization.framework Linux VM | Ephemeral macOS VMs (clone-on-write) |

A Windows box and a Mac Mini together cover every combination: linux/amd64, linux/arm64, windows/amd64.

## Build Matrix

Each OS/arch combination produces one self-contained binary with containerd compiled in:

| Target | Binary | How it runs containers |
|--------|--------|----------------------|
| linux/amd64 | `ephemerd` | containerd direct |
| linux/arm64 | `ephemerd` | containerd direct |
| windows/amd64 | `ephemerd.exe` | containerd + Hyper-V (Windows jobs) / WSL2 (Linux jobs) |
| darwin/arm64 | `ephemerd` | Virtualization.framework Linux VM + containerd inside |

No runtime dependencies beyond the OS kernel, Hyper-V (Windows), or Virtualization.framework (macOS).

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.26 |
| Container runtime | containerd v2 (in-process library) |
| macOS VM | Apple Virtualization.framework via [Code-Hex/vz](https://github.com/Code-Hex/vz/v3) |
| GitHub API | [go-github](https://github.com/google/go-github) v72 |
| Config | TOML ([BurntSushi/toml](https://github.com/BurntSushi/toml)) |
| Logging | slog (stdlib structured logging) |
| CLI | [urfave-cli/v3](https://github.com/urfave/cli) |
| Build system | [Mage](https://magefile.org/) |
| gRPC | google.golang.org/grpc (dispatch + control API) |
| Metrics | Prometheus via [client_golang](https://github.com/prometheus/client_golang) |
