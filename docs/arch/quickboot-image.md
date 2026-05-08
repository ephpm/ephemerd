# Quickboot Image: Custom Kernel + ephemerd-as-init

> **Status: proposal.** Not implemented. Scoping document for a possible future
> direction. Numbers are estimates from related projects (Firecracker, kata,
> Cirrus persistent worker), not measured.

## Context

Today the Linux VM boot pipeline (Vz on macOS, WSL2 on Windows) already does
most of the work a minimal-OS approach would do, just split across more layers
than it needs:

- **Kernel**: Alpine `linux-virt` APK, kernel extracted at build time
  (`mage/download/download.go` — `Kernel()`, `KernelLinux()`)
- **Initrd**: hand-built cpio with busybox + e2fsprogs + a pinned set of
  kernel modules (virtio, vsock, hyperv on x86, etc.)
- **Rootfs**: pre-baked Alpine tarball with gcompat + iptables (see
  [rootfs.md](rootfs.md))
- **Userspace**: initrd's `/init` formats `/dev/vda`, extracts the rootfs,
  pivots, and execs `ephemerd-linux` from a virtio-fs share

That stack works, but it's three handoffs (initrd → rootfs → ephemerd) and
two userspaces (busybox + Alpine) just to get to the only thing we actually
care about: ephemerd holding a containerd handle. WSL2 adds a fourth layer
on Windows (full Microsoft kernel + WSL init + Alpine rootfs + ephemerd).

The proposal: collapse this into one bootable artifact where the kernel
hands `/sbin/init` to ephemerd directly, and there is no rootfs pivot.

## Goals

1. **Sub-second boot** from VMM start to ephemerd grpc listening, on KVM/Vz.
2. **One artifact** per arch — `vmlinuz` + `initramfs.cpio.gz` (or a fused
   PE/EFI binary) — replacing kernel + initrd + rootfs tarball + linux binary.
3. **Smaller surface area** — no Alpine runtime, no busybox, no apk, no
   pivot_root step that can fail.
4. **Same job semantics** — the host-side scheduler, gRPC dispatch (Windows),
   and OCI runtime contract don't change.

Non-goals: replacing the macOS VM path, replacing WSL2 entirely (see
"Phased Rollout"), shipping our own libc.

## Architecture

```
┌────────────────────────────────────────────────────┐
│ vmlinuz  (custom Linux kernel, ~6-10 MB compressed)│
├────────────────────────────────────────────────────┤
│ initramfs.cpio.gz                                  │
│  ├── /init         -> ephemerd (statically linked) │
│  ├── /sbin/runc                                    │
│  ├── /opt/cni/bin/{bridge,host-local,loopback,...} │
│  ├── /usr/sbin/{iptables,ip6tables,nft}            │
│  ├── /lib/modules/<ver>/...   (only the few we use)│
│  └── /etc/{resolv.conf,nsswitch.conf}              │
└────────────────────────────────────────────────────┘
```

Total size budget: ~50-80 MB, vs. today's ~4 MB initrd + ~4 MB rootfs +
~50 MB linux ephemerd binary, but with no separate rootfs disk to format.

### ephemerd as PID 1

ephemerd already embeds containerd as a library and owns the runner +
networking lifecycle. As PID 1 it would additionally need to:

- Mount `/proc`, `/sys`, `/dev`, `/dev/pts`, `/sys/fs/cgroup` (cgroupv2
  unified)
- Bring up `lo`, configure `eth0` via DHCP or kernel cmdline args
- Read kernel cmdline (`/proc/cmdline`) for `ephemerd.containerd_port=`,
  `ephemerd.share_tag=`, etc. — same scheme as today's Vz init
- `reboot(LINUX_REBOOT_CMD_POWER_OFF)` on clean shutdown so the VMM exits

This is ~150-300 lines in a new `cmd/ephemerd/init_linux.go` behind a build
tag, gated by `os.Getpid() == 1`. Existing `serve` codepaths run unchanged
once the bootstrap is done.

### Kernel

Two viable sources, in increasing order of effort:

| Option | Effort | Boot time | Maintenance |
|---|---|---|---|
| **A.** Alpine `linux-virt` (today) | None | ~1.5 s cold on Vz | Alpine handles CVEs |
| **B.** Firecracker's published `microvm-kernel` configs | Low | ~125 ms on KVM | Track AWS' configs |
| **C.** Custom `make tinyconfig` + our drivers | Medium | ~80-150 ms | We own kernel CVEs |

Recommend starting at **A** (zero new build infra, just change userspace),
move to **B** when a measured boot-time problem justifies it. **C** is only
worth it if we're shipping to customers and need to defend the attack
surface, which we aren't.

### Initramfs construction

Keep it pure-Go like today's `mage download:rootfs`. New mage target:

```
mage build:quickboot
  → static-link ephemerd-linux (CGO_ENABLED=0, already done)
  → assemble cpio:
      /init  = ephemerd-linux
      /sbin/runc = (downloaded, pinned in mage/download)
      /opt/cni/bin/* = (downloaded)
      /usr/sbin/iptables* = (extracted from Alpine APK, today's path)
      /lib/modules/<ver>/* = (extracted from linux-virt APK)
  → gzip → initramfs.cpio.gz
  → embed via go:embed in pkg/vm/embed_<os>.go
```

No filesystem image, no `mkfs.ext4`, no virtio-fs share required for the
binary itself (still useful for the runner work directory and image cache —
see "Persistence" below).

### Persistence

The current `serve` mode keeps a 50 GB ext4 disk for containerd's image
cache so OCI pulls survive restarts. Quickboot keeps that — initramfs is
RAM only, but containerd's `--root` and `--state` paths point at a
virtio-block device mounted at `/var/lib/containerd`. The boot sequence is:

```
kernel → initramfs → ephemerd init:
  mount /proc /sys /dev
  mount /dev/vda /var/lib/containerd   (mkfs.ext4 first boot only)
  parse /proc/cmdline
  exec normal serve flow
```

`run` mode (one-shot workflow) skips the disk entirely — pure RAM, VM
disappears when done. This is how today's macOS `run` mode already works.

## Boot Time Estimate

Rough budget, KVM with `noapic noacpi quiet` and stripped initramfs:

| Stage | Today (Alpine+rootfs) | Quickboot |
|---|---|---|
| VMM → kernel jump | ~120 ms | ~120 ms |
| Kernel init | ~400 ms | ~250 ms (fewer drivers probed) |
| initrd → rootfs pivot | ~250 ms | 0 ms (no pivot) |
| Alpine OpenRC | ~600 ms | 0 ms (no init system) |
| ephemerd cold start | ~300 ms | ~250 ms |
| containerd ready | ~400 ms | ~400 ms |
| **Total to gRPC ready** | **~2.0 s** | **~1.0 s** |

Optimistic. Real measurement gates phase 2.

## Build Pipeline

```
mage build:linux         (existing) → ephemerd-linux static binary
mage download:kernel     (existing) → vmlinuz
mage download:cni        (existing) → CNI plugins
mage download:runc       (new)      → runc static binary
mage build:quickboot     (new)      → initramfs.cpio.gz
mage build:macos         → embeds quickboot artifacts
mage build:windows       → embeds quickboot artifacts (phase 3)
```

## Tradeoffs

**Wins**
- One artifact replaces four.
- Cuts roughly half the boot time (estimated; needs measurement).
- No Alpine package CVE noise in the runner VM image.
- Kills the initrd → rootfs handoff, which is the most failure-prone step in
  the current boot flow.

**Costs**
- We own the init contract. PID 1 dying = kernel panic. Today an OpenRC
  failure leaves us a debuggable shell; quickboot does not.
- Debugging needs serial console access from the start. No `ssh`, no `apk
  add gdb`. Practical answer: keep `busybox-static` at `/bin/busybox` in
  the initramfs and a `console=hvc0` getty as a fallback when
  `ephemerd.debug=1` is on the cmdline.
- Adding a new userspace tool (say, `nft` for a feature) means a rebuild
  of the embedded artifact, not an `apk add` at boot.
- Kernel CVEs: option A (Alpine kernel) keeps Alpine's cadence. Option B/C
  put us on the hook.

## Phased Rollout

1. **Phase 1 — macOS Linux VM.** Lowest blast radius: Vz Linux VM is
   already a single-purpose VM we control end to end. Build the quickboot
   artifact, wire `linuxvm_darwin.go` to boot it instead of the
   kernel+initrd+rootfs combo. Keep the Alpine path behind a feature flag
   for one release.
2. **Phase 2 — measure & tune.** Real boot-time numbers. Move to
   Firecracker-style kernel config only if the data says so.
3. **Phase 3 — Windows WSL2 replacement.** Harder: WSL2 has its own kernel
   and init that we don't fully control. Either run our quickboot image
   inside Hyper-V directly (bypasses WSL2 entirely — bigger lift, also
   eliminates the gRPC dispatch hop in [windows-single-scheduler.md](windows-single-scheduler.md)),
   or ship our initramfs as a WSL2 distro and accept WSL2's kernel. The
   former is the interesting version; the latter is a smaller win.

Phases 1 and 3 are independent — phase 1 ships value on its own.

## Open Questions

- Static-linking ephemerd against musl vs glibc. Today the binary is glibc
  (the rootfs ships gcompat for the same reason). Quickboot wants musl
  static so we can drop libc entirely from the image. Likely needs a
  separate cross-compile target.
- Do we want `/sbin/init` to be ephemerd directly, or a tiny shim that
  execs ephemerd? Shim is ~30 lines and survives ephemerd panics long
  enough to dump a stack to the serial console. Probably worth it.
- Console behavior on panic. Real OSes log to syslog over network; we
  don't have one. Probably: `ephemerd.crashdump=virtio-blk:/dev/vdb`
  writes a panic log to a known offset on a small dedicated device.
- ARM64 vs x86_64. macOS is ARM64-only; Windows is x86_64; Linux servers
  are both. Build both arches in the same target.

## Why not a unikernel

OSv / Nanos / IncludeOS would let us skip Linux entirely — ephemerd compiles
to a guest. Killer for boot time (~10 ms), kills containerd: every container
runtime we'd want to support assumes Linux syscalls and namespaces. Not a
viable path for a CI runner. Mentioned only because it's the natural next
question.

## Key Files (when implemented)

| File | Purpose |
|------|---------|
| `cmd/ephemerd/init_linux.go` | PID-1 detection, mount setup, cmdline parse |
| `mage/build/quickboot.go` | Initramfs cpio assembly |
| `mage/download/runc.go` | Pinned runc static binary download |
| `pkg/vm/embed_darwin.go` | Embeds quickboot artifact for Vz |
| `pkg/vm/linuxvm_darwin.go` | Boots quickboot instead of kernel+initrd+disk |
| `docs/arch/quickboot-image.md` | This document |
