# macOS Architecture: Linux + macOS Runners via Virtualization.framework

## Overview

On macOS (Apple Silicon), ephemerd runs both Linux and macOS CI jobs using Apple's Virtualization.framework (Vz). Two VM types serve different purposes:

- **Linux VM** — lightweight Alpine VM running containerd for OCI container jobs
- **macOS VM** — ephemeral clone-on-write macOS instances for native Xcode/Swift jobs

Both use the same Vz hypervisor, hardware-accelerated on Apple Silicon.

```
macOS Host (ephemerd):
  +-- Linux VM (Vz, Alpine, containerd inside)
  |   +-- Linux jobs run as OCI containers
  |
  +-- macOS VMs (Vz, per-job APFS clones)
      +-- macOS jobs run natively (Xcode, Swift, etc.)
```

## Linux VM

### What's Inside

The Linux VM runs a minimal Alpine system with:

- **ephemerd** (Linux binary, run from virtio-fs share)
- **containerd** (in-process, same as native Linux)
- **gcompat + iptables** (pre-baked into rootfs at build time, see [rootfs.md](rootfs.md))
- **CNI plugins + containerd-shim-runc-v2** (extracted at startup)

### Boot Assets

| Asset | Format | Source |
|-------|--------|--------|
| `vmlinuz` | Linux kernel image | Downloaded via `ephemerd vm setup` |
| `initrd` | initramfs (cpio) | Downloaded or built at compile time |
| `ephemerd-rootfs-*.tar.gz` | Alpine rootfs tarball | Built at compile time by `mage download:rootfs` |
| `ephemerd-linux` | Static Linux binary | Cross-compiled by `mage build:linux` |

The rootfs tarball and Linux binary are embedded in the macOS binary via `go:embed`. The kernel and initrd are either embedded or downloaded on first run.

### Kernel Command Line

```
console=hvc0 root=/dev/vda rw ephemerd.containerd_port=10000 ephemerd.share_tag=ephemerd quiet
```

- `root=/dev/vda` — root filesystem is on the virtio block device (raw disk image)
- `ephemerd.share_tag=ephemerd` — tells init to mount the virtio-fs share at `/mnt/ephemerd`
- `ephemerd.containerd_port` — TCP port containerd listens on (host connects via NAT)

### Host ↔ VM Communication

- **virtio-fs**: host's `DataDir` is shared into the VM at `/mnt/ephemerd`. The ephemerd Linux binary lives here — no need to copy it into the disk image. It loads into memory on exec and runs at native speed.
- **TCP over NAT**: containerd inside the VM listens on a TCP port. The host connects a gRPC containerd client to `127.0.0.1:<port>`.

### Two Boot Modes

The Linux VM serves both `serve` and `run` commands but with different lifecycle and storage requirements.

#### `serve` — Long-Running, Persistent Disk

```
Build time:
  rootfs tarball ──embed──> macOS binary
  ephemerd-linux ──embed──> macOS binary

First boot:
  extract rootfs tarball + ephemerd-linux to DataDir
  create sparse raw disk image (50 GB)
  initrd formats /dev/vda as ext4, extracts rootfs onto it
  boot into /dev/vda, mount virtio-fs
  run ephemerd-linux from /mnt/ephemerd/

Subsequent boots:
  /dev/vda already populated, just mount and go
```

The persistent disk gives containerd a durable image cache and snapshotter store. Pulled OCI images survive across restarts — no re-pulling on reboot.

#### `run` — Ephemeral, Initramfs Root

```
Build time:
  rootfs tarball ──convert to cpio──> initramfs ──embed──> macOS binary
  ephemerd-linux ──embed──> macOS binary

Each run:
  extract initramfs + kernel + ephemerd-linux to DataDir
  boot Vz VM (kernel + initramfs, NO disk image)
  root filesystem is tmpfs (RAM-backed)
  virtio-fs shares DataDir → /mnt/ephemerd
  run ephemerd-linux from /mnt/ephemerd/
  job runs, VM tears down, nothing persists
```

No disk image means no first-boot formatting step, no cleanup, and no leftover state. The VM boots, runs one job, and disappears. Total lifecycle is ~5-10 seconds for boot + containerd ready.

### Why Two Modes

| | `serve` (persistent disk) | `run` (initramfs) |
|---|---|---|
| **Lifecycle** | Boots once, runs for hours/days | Boots per invocation, ~seconds |
| **Image cache** | Persists across restarts | Gone when VM exits |
| **Disk cleanup** | None needed | None needed (no disk) |
| **First boot** | Slower (format + extract) | Same speed every time |
| **Use case** | Production CI polling | Local dev "run this workflow" |

## macOS VM

### How It Works

macOS-native jobs (Xcode builds, Swift tests, etc.) run inside ephemeral macOS VMs. Each job gets a fresh VM cloned from a provisioned base image.

```
Base image (provisioned once):
  macOS installed via 'ephemerd vm setup-macos'
  Xcode, CLI tools, GitHub runner pre-installed
  Stored at vm.macos.base_image config path

Per-job:
  APFS clone (cp -c) of base image → instant, near-zero I/O
  Boot Vz VM from clone (MacOSBootLoader, Apple Silicon platform)
  Job runs natively inside macOS
  VM stops, clone deleted
```

APFS clone-on-write means the per-job copy is nearly instant and only allocates disk space for writes. A 60 GB base image produces a clone in milliseconds.

### macOS VM Configuration

- **Platform**: `MacPlatformConfiguration` (required for macOS guests on Apple Silicon)
- **Graphics**: `MacGraphicsDeviceConfiguration` with 1920x1200 display (required even headless)
- **Networking**: NAT via Vz (VM gets IP, can reach internet)
- **Storage**: Virtio block device pointing to the APFS clone

### Runner Integration

The base image includes a pre-configured GitHub Actions runner. On boot, the runner starts automatically and picks up the JIT config injected by ephemerd. IP discovery for connecting to the runner uses mDNS/Bonjour (`.local` addresses).

## Job Routing

The scheduler routes jobs based on `runs-on` labels:

| `runs-on` | Where it runs |
|-----------|--------------|
| `[self-hosted, linux, x64]` | OCI container inside Linux VM |
| `[self-hosted, linux, arm64]` | OCI container inside Linux VM (ARM native) |
| `[self-hosted, macos, arm64]` | Ephemeral macOS VM (APFS clone) |

On macOS there's no dispatch architecture like Windows has. The Linux VM's containerd is accessed directly via TCP — the host binary can create Linux containers because the gRPC containerd client is platform-agnostic. Only the container runtime code (OCI spec, snapshotter, networking) runs inside the VM.

## Build Pipeline

```
mage build:macos
  1. mage download:rootfs          — Alpine + gcompat + iptables tarball (pure Go)
  2. Cross-compile ephemerd-linux   — GOOS=linux GOARCH=arm64
  3. Build macOS binary             — embeds rootfs + linux binary
```

The macOS binary embeds everything needed for Linux jobs. macOS base images are provisioned separately via `ephemerd vm setup-macos` (one-time setup).

## Key Files

| File | Purpose |
|------|---------|
| `pkg/vm/vm.go` | `LinuxVM` and `MacOSVM` interfaces |
| `pkg/vm/linuxvm_darwin.go` | Vz Linux VM: boot, virtio-fs, containerd wait |
| `pkg/vm/macosvm_darwin.go` | Vz macOS VM: APFS clone, per-job lifecycle |
| `pkg/vm/embed_darwin.go` | `go:embed` directives for rootfs + linux binary |
| `cmd/ephemerd/runtime_darwin.go` | `startContainerRuntime()`: boots Linux VM for `serve` |
| `mage/download/download.go` | `Rootfs()`: builds pre-baked Alpine rootfs |
