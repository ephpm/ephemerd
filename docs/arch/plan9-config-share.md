# Plan9 Host-Config Share

> **Status: implemented.** The in-VM ephemerd now reads its config from
> a read-only Plan9 share that exposes the host's data dir over Hyper-V
> VMBus. Adding a new in-VM-relevant config knob no longer requires
> kernel cmdline plumbing or an in-VM ephemerd flag.

## Context

Until now, every host-side setting that needed to take effect *inside
the Linux VM* required its own ad-hoc plumbing across the VM boundary:

1. A field on `vm.LinuxVMConfig` (`DindEnabled`, `DindAllowPrivileged`).
2. A kernel command-line parameter (`ephemerd.dind=1`,
   `ephemerd.dind_allow_privileged=1`) appended by
   `pkg/vm/linuxvm_windows.go`.
3. A parser for that parameter in the init script
   (`mage/download/download.go`).
4. A CLI flag on `ephemerd serve` (`--dind-allow-privileged`) that
   overrides the in-VM config.
5. A re-render of the init script + initrd, and a rebuild of the host
   binary.

PRs #87 (metrics — needed `container_stats_interval` over the boundary
for the in-VM sampler) and #88 (dind allow-privileged plumbing) both
had to walk this path. The cost-per-knob is small but real, and the
pattern doesn't scale: every future in-VM-relevant setting drags four
files plus an arch decision about whether it's worth the work.

The setup also has a latent footgun. The initrd cache only invalidated
when the rootfs tarball changed (#88 fixed this), so a fresh `mage
build:windows` could happily embed a stale init script and an operator
would see "I set the new option in config.toml, restarted ephemerd, and
nothing changed."

## What this changes

The host's data directory (`C:\ProgramData\ephemerd` on Windows) is now
exposed to the Linux VM as a **read-only Plan9 share** named
`ephemerd-host-config`. The init script mounts the share at
`/mnt/host-config` and points the in-VM `ephemerd serve` at
`/mnt/host-config/config.toml` via `--config`. The in-VM daemon then
reads the same TOML the host reads, every boot.

## Why Plan9 specifically

Hyper-V's guest-host file sharing for Linux VMs uses the **9P2000.L**
protocol over VMBus. The kernel surface we need
(`CONFIG_NET_9P_VIRTIO`, `CONFIG_9P_FS`) is small and was already
compiled into the embedded `linux-virt` kernel. The modules
(`9pnet.ko`, `9pnet_virtio.ko`, `9p.ko`) were already listed in
`initrdKernelModulesX86` — someone wired them at kernel-build time
but never wired the host. We just connect the dots.

virtio-fs would be the natural choice on KVM/QEMU, but Hyper-V doesn't
implement it. SMB into the VM via the network adapter would work but
adds an interface, a routing decision, and credentials. Plan9 is point-
to-point, authenticated by the hypervisor, and doesn't touch the netns.

## Failure mode + fallback

If the share fails to mount (kernel without 9p modules, share not
exported, mount returns non-zero) the init script logs a warning and
proceeds without `--config`. The in-VM ephemerd falls back to its
defaults baked into the VHDX and the kernel-cmdline `ephemerd.dind=*`
parameters still flip the dind-related bits. So a stripped kernel or a
share misconfiguration degrades gracefully back to today's behavior.

The kernel-cmdline plumbing from #88 is **deliberately retained** as
this fallback. It's redundant when the share is healthy (the same
values come through the shared config) but free to keep.

## Security boundary

The share is **read-only** from the VM's perspective. A compromised
in-VM ephemerd cannot mutate the host's config.toml or any other file
on the host. Job containers (the threat surface) run inside the in-VM
containerd's own mount namespace and never see `/mnt/host-config` at
all — they get only the bind mounts the runtime hands them, which is
just `<host data dir>/runner` (the per-job runner copy) and
`<host data dir>/hosts` / `<dns>` (per-job network config).

The host data dir can contain things other than config.toml — image
tarballs, containerd state, runner extractions. None of those have
secrets; the GitHub App private key lives outside the data dir (its
path is named in `config.toml`, but the file itself isn't mirrored
into the VM). Webhook secrets and similar live inline in
`config.toml`, so they *are* visible to the in-VM ephemerd — but that
process is already trusted (it's a peer of the host ephemerd, talks
to it via authenticated gRPC, and runs the same code).

## What gets read inside the VM

The in-VM ephemerd's worker-mode code path
(`--containerd-only`) dereferences a narrow slice of the config:

- `[dind]` — `enabled`, `allow_privileged`, cache settings.
- `[runtime.rlimits]` — per-container nofile, etc.
- `[log]` — log level/format.

Everything else (`[github]`, `[runner.windows]`, `[metrics]`,
`[vm.linux]`, `[webhook]`, tunnel configs, repo allowlists) is parsed
into the in-memory `cfg` but never read in worker mode. So sharing the
whole config is safe — irrelevant sections sit inert.

The one section worth being deliberate about is `[metrics]`. If we
ever wire an "auto-start a metrics server when `enabled`" path into
the worker block, an in-VM daemon reading the host config would
suddenly spin up a duplicate `/metrics` endpoint inside the VM. The
worker block today doesn't, by design — the host scrapes in-VM
container stats via the Dispatch stream (#87) precisely so the VM
doesn't need its own listener. Future changes to worker mode should
keep this invariant.

## Removing the cmdline-per-knob pattern (eventually)

This doesn't strip the `ephemerd.dind*` cmdline plumbing yet — it's
still there as the fallback. Once the share has soaked for a release
or two and we're confident no operators rely on a 9p-less kernel, the
cmdline params and the `--dind-allow-privileged` CLI flag can be
deleted in a follow-up. The dind PR's existence becomes a footnote.

## Not in scope

- **macOS**: the Darwin Vz Linux VM uses virtio-fs (Vz exposes it
  directly); a symmetric path exists but the wiring is different. The
  same shape (share the host data dir read-only, mount in init,
  `--config`) applies. Tracked separately.
- **Linux host**: ephemerd on Linux runs the container runtime
  directly (no in-VM daemon to share with). No-op.
- **Read-write share**: deliberately no. The host owns the config; the
  VM never writes to it. If the in-VM ephemerd ever needs to persist
  state visible to the host, that's a separate, narrower share.

## Failure modes worth knowing

- **Kernel without 9p**: graceful fallback to defaults + cmdline.
  Logged at WARN.
- **config.toml missing from data dir**: in-VM falls back to defaults.
  Logged.
- **Share mounted but file not readable** (permissions, weird Windows
  ACL): init script's `[ -f ]` check skips `--config` — logged, no
  fall back to the cmdline path.
- **Operator edits config.toml while the VM is running**: not picked
  up until the next VM boot. Restart ephemerd.

## File pointers

- Host side: `pkg/vm/linuxvm_windows.go` populates `Plan9.Shares`
- Cmdline param: `pkg/vm/linuxvm_windows.go` cmdline construction
- VM mount: init script in `mage/download/download.go`
- Existing 9p kernel modules: `initrdKernelModulesX86` in
  `mage/download/download.go`
- The existing Plan9 struct: `pkg/vm/hcs_windows.go:128`
