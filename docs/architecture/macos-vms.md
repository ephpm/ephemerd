---
title: macOS VMs
weight: 4
---

On macOS (Apple Silicon), ephemerd runs both Linux and macOS CI jobs using Apple's Virtualization.framework (Vz). Two VM types serve different purposes:

- **Linux VM** -- lightweight Alpine VM running containerd for OCI container jobs.
- **macOS VM** -- ephemeral clone-on-write macOS instances for native Xcode/Swift jobs.

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

### What Runs Inside

The Linux VM runs a minimal Alpine system with:

- **ephemerd** (Linux binary, run from virtio-fs share)
- **containerd** (in-process, same as native Linux)
- **gcompat + iptables** (pre-baked into rootfs at build time, see [Pre-baked rootfs]({{< relref "pre-baked-rootfs" >}}))
- **CNI plugins + containerd-shim-runc-v2** (extracted at startup)

### Boot Assets

| Asset | Format | Source |
|-------|--------|--------|
| `vmlinuz` | Linux kernel image | Downloaded via `ephemerd vm setup` |
| `initrd` | initramfs (cpio) | Downloaded or built at compile time |
| `ephemerd-rootfs-*.tar.gz` | Alpine rootfs tarball | Built at compile time by `mage download:rootfs` |
| `ephemerd-linux` | Static Linux binary | Cross-compiled by `mage build:linux` |

The rootfs tarball and Linux binary are embedded in the macOS binary via `go:embed`. The kernel and initrd are either embedded or downloaded on first run.

### Host-to-VM Communication

- **virtio-fs**: the host's data directory is shared into the VM at `/mnt/ephemerd`. The ephemerd Linux binary lives here -- no need to copy it into the disk image. It loads into memory on exec and runs at native speed.
- **TCP over NAT**: containerd inside the VM listens on a TCP port. The host connects a gRPC containerd client to `127.0.0.1:<port>`.

Unlike Windows WSL dispatch, macOS does not need a separate dispatch layer. The containerd gRPC client is platform-agnostic -- the macOS host binary can create Linux containers directly via the TCP connection. Only the container runtime code (OCI spec, snapshotter, networking) runs inside the VM.

### Two Boot Modes

#### `serve` -- Long-Running, Persistent Disk

The persistent disk gives containerd a durable image cache and snapshotter store. Pulled OCI images survive across restarts. On first boot, the initrd formats `/dev/vda` as ext4 and extracts the rootfs onto it. Subsequent boots skip this step.

#### `run` -- Ephemeral, Initramfs Root

No disk image. The root filesystem is RAM-backed (tmpfs). The VM boots, runs one job, and disappears. Total lifecycle is ~5-10 seconds for boot + containerd ready. No cleanup needed.

| | `serve` (persistent disk) | `run` (initramfs) |
|---|---|---|
| Lifecycle | Boots once, runs for hours/days | Boots per invocation, ~seconds |
| Image cache | Persists across restarts | Gone when VM exits |
| First boot | Slower (format + extract) | Same speed every time |
| Use case | Production CI polling | Local dev "run this workflow" |

## macOS VM

### How It Works

macOS-native jobs (Xcode builds, Swift tests, code signing) run inside ephemeral macOS VMs. Each job gets a fresh VM APFS-cloned from a provisioned base disk image.

Two images are involved in each job:

- **macOS VM disk image** (`<data_dir>/vm/macos/base.img`): a Vz-bootable macOS install pulled from a Tart OCI image. This is what the VM boots from.
- **OCI base image** (per-job): release artifacts or toolchains pulled from a container registry and overlaid onto the running VM via virtio-fs.

### Per-Job VM Lifecycle

```
Per-job (~36s total):
  1. APFS clone (cp -c) of base.img         -- instant, near-zero I/O
  2. Mount clone, inject runner + JIT config -- ~4s (host-side cp -R)
  3. Boot Vz VM from clone                   -- ~12s to SSH ready
  4. SSH in: firewall + start runner + harden -- ~3s + 3s settle
  5. Job runs natively inside macOS
  6. VM stops, clone deleted                 -- zero leftover state
```

APFS clone-on-write means the per-job copy is nearly instant and only allocates disk space for writes. A 40 GB disk image produces a clone in milliseconds.

### Tart OCI Base Image

The first time ephemerd starts on a Mac, it pulls a pre-built macOS VM image from the [Tart](https://github.com/cirruslabs/tart) OCI registry. The image is selected automatically based on the host's macOS version (e.g., macOS 26 maps to `ghcr.io/cirruslabs/macos-tahoe-vanilla:latest`). This is a one-time operation -- subsequent daemon restarts skip it because `base.img` already exists.

The Tart vanilla images ship with Setup Assistant completed, SSH enabled, an `admin` user with passwordless sudo, and auto-login configured.

The pull downloads LZ4-compressed disk chunks (~5-8 GB total for a ~40 GB sparse disk) and decompresses them using Apple's Compression framework. Linux jobs still work during the pull -- the scheduler starts immediately and the pull runs in the background. To re-pull, delete the `vm/macos/` directory.

To supply your own image instead (e.g., one with Xcode pre-installed), set `vm.macos.disk_image` in `config.toml` and the pull is skipped entirely.

### Runner Injection

The GitHub Actions runner is injected into each per-job VM clone before boot. The APFS clone is mounted on the host via `hdiutil`, and the runner binary plus JIT config are written directly to the filesystem at `/Users/admin/actions-runner/`. This host-side injection takes ~4s and avoids the overhead of transferring ~500 MB over SSH after boot.

After the VM boots and SSH becomes available (~12s), ephemerd SSHes in and starts the runner with the pre-injected JIT config.

IP discovery uses ARP table lookup -- ephemerd records the VM's MAC address at creation time, then probes the Vz NAT subnet and scans `arp -an` output to find the corresponding IP.

### Per-Job Security Hardening

Each per-job VM is hardened before any job code runs:

1. **Password randomized** -- the `admin` password is replaced with 32 random bytes from `/dev/urandom`.
2. **SSH locked to key-only** -- password authentication is disabled in `sshd_config`. Only the ephemeral SSH key generated in-memory by this ephemerd session can connect.
3. **Ephemeral SSH key** -- a fresh ed25519 key pair is generated on every ephemerd restart (never written to disk). The public key is injected into the VM's `authorized_keys` via the virtio-fs share.
4. **VM destroyed after job** -- the APFS clone and all per-job state are deleted when the job completes or times out.

### Network Isolation

Each macOS VM gets a `pfctl` firewall configured via SSH before the runner starts:

- **Blocked**: all RFC 1918 private networks (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and link-local (169.254.0.0/16). This prevents jobs from reaching the host, other VMs, or the local network.
- **Allowed**: DNS/DHCP to the Vz NAT gateway (192.168.64.1), and all public internet traffic.

This matches the isolation model for Linux container jobs, which use iptables rules via CNI.

### SSH Debugging

`ephemerd jobs ssh <job-id>` opens an interactive SSH session to a running macOS VM. The command connects to the daemon's control socket, retrieves the ephemeral SSH key and VM IP, and proxies a terminal session. No SSH keys are stored on disk.

## Job Routing

| `runs-on` | Where it runs |
|-----------|--------------|
| `[self-hosted, linux, x64]` | OCI container inside Linux VM |
| `[self-hosted, linux, arm64]` | OCI container inside Linux VM (ARM native) |
| `[self-hosted, macos, arm64]` | Ephemeral macOS VM (APFS clone) |

## Key Files

| File | Purpose |
|------|---------|
| `pkg/vm/vm.go` | `LinuxVM` and `MacOSVM` interfaces, ephemeral SSH key generation |
| `pkg/vm/linuxvm_darwin.go` | Vz Linux VM: boot, virtio-fs, containerd wait |
| `pkg/vm/macosvm_darwin.go` | Vz macOS VM: APFS clone, runner injection, SSH setup |
| `pkg/vm/macos_install_darwin.go` | Tart OCI image pull, LZ4 decompression |
| `pkg/vm/embed_darwin.go` | `go:embed` directives for rootfs + Linux binary |
| `cmd/ephemerd/ssh.go` | `jobs ssh <id>` CLI for interactive SSH |
| `cmd/ephemerd/runtime_darwin.go` | `startContainerRuntime()`: boots Linux VM for `serve` |
