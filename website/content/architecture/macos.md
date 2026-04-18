---
title: "^# "
---


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

macOS-native jobs (Xcode builds, Swift tests, etc.) run inside ephemeral macOS VMs. Each job gets a fresh VM APFS-cloned from a provisioned macOS **disk image**.

Two images are involved in each job — don't confuse them:

- **macOS VM disk image** (`<data_dir>/vm/macos/base.img`): a Vz-bootable macOS install pulled from a Tart OCI image. This is what the VM boots from. Configured via `vm.macos.disk_image`.
- **OCI base image** (per-job): release artifacts / toolchains (Xcode, Swift SDK, whatever the job needs) pulled from a container registry and overlaid onto the running VM via virtio-fs. Configured per job via the workflow's image label.

```
macOS VM disk image (pulled once from Tart registry on first boot):
  ghcr.io/cirruslabs/macos-<version>-vanilla:latest
  Pre-configured: SSH enabled, admin user, sudo, auto-login

Per-job (~36s total):
  1. APFS clone (cp -c) of base.img         — instant, near-zero I/O
  2. Mount clone, inject runner + JIT config — ~4s (host-side cp -R)
  3. Boot Vz VM from clone                   — ~12s to SSH ready
  4. SSH in: firewall + start runner + harden — ~3s + 3s settle
  5. Job runs natively inside macOS
  6. VM stops, clone deleted                 — zero leftover state
```

APFS clone-on-write means the per-job copy is nearly instant and only allocates disk space for writes. A 40 GB disk image produces a clone in milliseconds.

### Per-job VM boot sequence

Each macOS job goes through these stages:

1. **APFS clone** — `cp -c` of `base.img` to a per-job `.img` file. Copy-on-write, milliseconds.
2. **Runner injection** — the clone is mounted on the host via `hdiutil`, and the GitHub Actions runner + JIT config are written directly to the filesystem (`/Users/admin/actions-runner/` and `/tmp/ephemerd/.jit_config`). This takes ~4s and avoids the need to copy files over SSH after boot.
3. **VM boot** — the clone boots with 2 CPUs and 2 GB RAM (configurable). SSH is available in ~12s thanks to the Tart vanilla image having Remote Login pre-enabled.
4. **SSH setup** — ephemerd SSHes into the VM using the ephemeral key (with `admin/admin` password fallback) and runs a setup script that:
   - Configures a `pfctl` firewall blocking private networks
   - Starts the runner in the background (`./run.sh --jitconfig ...`)
   - Randomizes the admin password (last step, so the session isn't killed mid-setup)
5. **Job execution** — the runner connects to GitHub and executes the workflow job.
6. **Cleanup** — the VM is stopped, the APFS clone and aux storage are deleted. No state persists between jobs.

### First-boot base image pull

The first time ephemerd starts on a Mac, it pulls a pre-built macOS VM image from the [Tart](https://github.com/cirruslabs/tart) OCI registry. The image is selected automatically based on the host's macOS version (e.g. macOS 26 → `ghcr.io/cirruslabs/macos-tahoe-vanilla:latest`). This is a one-time operation — subsequent daemon restarts skip it because `base.img` already exists.

The Tart vanilla images ship with Setup Assistant completed, SSH enabled, an `admin` user with passwordless sudo, and auto-login configured. Ephemerd injects a runner startup LaunchDaemon into the image after pulling.

The pull downloads LZ4-compressed disk chunks (~5-8 GB total for a ~40 GB sparse disk) and decompresses them using Apple's Compression framework. Progress is logged per layer. Typical output:

```
msg="pulling macOS base image from Tart OCI registry" image=ghcr.io/cirruslabs/macos-tahoe-vanilla:latest
msg="pulling disk layer" layer=1/94 size_mb=3
msg="pulling disk layer" layer=50/94 size_mb=497
msg="disk image assembled" path=/var/lib/ephemerd/vm/macos/base.img
msg="wrote LaunchDaemon for runner startup"
msg="macOS base image ready"
```

**Ephemerd will not accept macOS jobs during this window.** Linux jobs still work — the scheduler starts immediately and the pull runs in the background. Subsequent daemon restarts see `base.img` and skip the pull entirely. To re-pull, delete the `vm/macos/` directory.

To supply your own image instead (e.g. one with Xcode pre-installed), set `vm.macos.disk_image` in `config.toml` and the pull is skipped entirely.

### Per-job VM security hardening

Each per-job VM is an APFS clone of the base image. The base image ships with a known default password (`admin/admin` from Tart), but per-job VMs are hardened on every boot by the runner LaunchDaemon before any job code runs:

1. **Password randomized** — the `admin` password is replaced with 32 random bytes from `/dev/urandom`. The default password is never exposed on the network.
2. **SSH locked to key-only** — password authentication is disabled in `sshd_config`. Only the ephemeral SSH key generated in-memory by this ephemerd session can connect.
3. **Ephemeral SSH key** — a fresh ed25519 key pair is generated on every ephemerd restart (never written to disk). The public key is injected into the VM's `authorized_keys` via the virtio-fs share. When ephemerd restarts, the old key is gone — no stale keys accumulate.
4. **VM destroyed after job** — the APFS clone and all per-job state (including the randomized password and SSH key) are deleted when the job completes or times out.

The base image itself retains the default password, but it is never booted directly — only cloned. The clone is hardened before any job code runs.

### Network isolation

Each macOS VM gets a `pfctl` firewall configured via SSH before the runner starts:

- **Blocked**: all RFC 1918 private networks (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and link-local (169.254.0.0/16). This prevents jobs from reaching the host, other VMs on the same NAT subnet, or the local network.
- **Allowed**: DNS/DHCP to the Vz NAT gateway (192.168.64.1), and all public internet traffic.

This matches the isolation model for Linux container jobs, which use iptables rules via CNI to block private network access.

### macOS VM Configuration

- **Platform**: `MacPlatformConfiguration` (required for macOS guests on Apple Silicon)
- **Graphics**: `MacGraphicsDeviceConfiguration` with 1920x1200 display (required even headless)
- **Networking**: NAT via Vz (VM gets IP, can reach internet)
- **Storage**: Virtio block device pointing to the APFS clone

### Runner Integration

The GitHub Actions runner is injected into each per-job VM clone before boot by mounting the APFS clone on the host and copying the extracted runner into `/Users/admin/actions-runner/`. The JIT config is written to `/tmp/ephemerd/.jit_config`. This host-side injection takes ~4s and avoids the overhead of transferring ~500 MB over SSH after boot.

After the VM boots and SSH becomes available (~12s), ephemerd SSHes in and starts the runner with the pre-injected JIT config. The host writes a `.ready` file to the job's shared directory once the runner is started. SSH port 22 readiness (Tart vanilla has it pre-enabled) is the primary readiness signal.

IP discovery uses ARP table lookup — ephemerd records the VM's MAC address at creation time, then probes the Vz NAT subnet and scans `arp -an` output to find the corresponding IP. MAC addresses are normalized (zero-padded) to handle format differences between Vz and macOS ARP output.

#### Debugging running VMs

`ephemerd jobs ssh <job-id>` opens an interactive SSH session to a running macOS VM. The command connects to the daemon's control socket, retrieves the ephemeral SSH key and VM IP, and proxies a terminal session. No SSH keys are stored on disk — the key exists only in the daemon's memory for the lifetime of the process.

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

The macOS binary embeds everything needed for Linux jobs. macOS VM disk images are pulled automatically from the Tart OCI registry on first boot.

## Key Files

| File | Purpose |
|------|---------|
| `pkg/vm/vm.go` | `LinuxVM` and `MacOSVM` interfaces, ephemeral SSH key generation |
| `pkg/vm/linuxvm_darwin.go` | Vz Linux VM: boot, virtio-fs, containerd wait |
| `pkg/vm/macosvm_darwin.go` | Vz macOS VM: APFS clone, runner injection, SSH setup, per-job lifecycle |
| `pkg/vm/macos_install_darwin.go` | Tart OCI image pull, LZ4 decompression, LaunchDaemon injection |
| `pkg/vm/embed_darwin.go` | `go:embed` directives for rootfs + linux binary |
| `pkg/scheduler/vmssh.go` | VM SSH info HTTP endpoint for `jobs ssh` command |
| `cmd/ephemerd/ssh.go` | `jobs ssh <id>` CLI — interactive SSH into running macOS VMs |
| `cmd/ephemerd/runtime_darwin.go` | `startContainerRuntime()`: boots Linux VM for `serve` |
| `mage/download/download.go` | `Rootfs()`: builds pre-baked Alpine rootfs |

## Future Work

### Pre-baked runner images (~15s boot target)

The current ~36s boot-to-ready time is dominated by two steps: runner injection (~4s) and SSH setup (~13s, of which 3s is an artificial sleep). Publishing ephemerd-specific Tart images with the runner pre-installed would eliminate the injection step entirely and let us start the runner immediately on boot.

The pipeline would be:

1. Pull the Tart vanilla image monthly (same cadence as Cirrus Labs)
2. Boot it, install the runner via SSH, shut down
3. Push to `ghcr.io/ephpm/macos-<version>-runner:latest`
4. ephemerd pulls this image instead of vanilla

With the runner pre-baked, the per-job flow becomes: clone → boot → SSH → start runner (~15s). The runner start could eventually move to a LaunchDaemon in the baked image, eliminating the SSH step entirely and getting close to ~12s (just boot + runner auto-start).

### OCI layer-level resume

The Tart image pull downloads 94 LZ4 disk layers sequentially. If the daemon restarts mid-pull, all layers are re-downloaded. Adding per-layer tracking (a manifest of completed digests) would allow resuming from the last completed layer.

### Parallel layer downloads

Layers are currently downloaded sequentially. Downloading 2-3 layers concurrently (with sequential disk writes) would cut the initial pull time significantly on high-bandwidth connections.
