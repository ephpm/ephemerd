# Host Config Delivery via Boot-Initrd Tail

> **Status: implemented.** The in-VM ephemerd reads the host's
> `config.toml`, delivered on every VM boot through the same
> runtime-generated initrd tail that carries `ephemerd-linux`. Adding a
> new in-VM-relevant config knob costs zero plumbing: edit the host's
> config.toml, restart ephemerd, the VM reboots and reads the same TOML.

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
pattern doesn't scale.

## The mechanism

ephemerd already rebuilds the boot initrd **on every VM start**:
`pkg/vm.buildBootInitrd` appends a small gzipped cpio tail containing
`/assets/ephemerd-linux` to the build-time base initrd. The kernel
concatenates initrd cpio archives, so files in the appended tail
override or add to the base. That is how a fresh `go build` of
ephemerd.exe delivers a new Linux binary into the VM without an
initrd rebuild.

This feature adds one more file to that tail: the host's
`config.toml`, when it exists, lands at `/assets/config.toml`. The
init script stages it to `/etc/ephemerd/config.toml` (mode 0600) and
passes `--config /etc/ephemerd/config.toml` to the in-VM `ephemerd
serve`. The in-VM daemon then reads the same TOML the host reads.

Because the tail is regenerated on every VM boot and a VM boot happens
on every ephemerd service start, "edit config.toml + restart the
service" is the complete update procedure. Same semantics as the host
daemon itself.

## Why not a live file share (Plan9 / virtio-fs)

The first draft of this feature exposed the host data dir to the VM as
a Hyper-V Plan9 share. It failed in two independent ways, the first of
which took down Linux CI on the dev rig for ~100 minutes:

1. **The HCS document was rejected at VM start** (`HcsStartComputeSystem:
   HRESULT 0xc0370110`) — the `Plan9` device JSON we constructed did not
   match the schema HCS expects at creation time. The VM never booted,
   ephemerd logged a single WARN, and every `[self-hosted linux x64]`
   job sat queued while the host poll loop skipped them with "OS labels
   don't match this platform."
2. **More fundamentally, the guest could never have mounted it.**
   Hyper-V serves Plan9 shares over **hvsock**, not virtio — there is no
   virtio-9p device on HCS. Mainline `mount -t 9p` supports
   `trans=virtio|tcp|fd|...` but has no hvsock transport; LCOW's GCS
   daemon makes this work by opening an `AF_VSOCK` socket itself and
   passing the fd via `trans=fd`. Replicating that means a vsock dialer
   + mount helper in the guest — real machinery, for a file we read
   exactly once at boot.

A live share buys *continuous* visibility of host files. We need a
*boot-time snapshot* of one file. The initrd tail already exists, is
exercised on every boot, has no new kernel or transport surface, and
fails in exactly one obvious way (file missing → defaults).

virtio-fs is the natural choice on Apple Vz for the Darwin equivalent
(Vz exposes virtio-fs directly) — that remains the plan for macOS,
tracked separately.

## Security

- `config.toml` can contain webhook secrets. It is written into the
  cpio tail with mode 0600 and staged in the VM at
  `/etc/ephemerd/config.toml` with mode 0600, root-owned. Job
  containers never see the VM's host rootfs — they get only the bind
  mounts the runtime hands them.
- The boot initrd lives at `<data-dir>\vm\linux\initrd` on the host —
  the same ACL domain as `config.toml` itself, so embedding the config
  does not widen host-side exposure.
- The GitHub App private key is **not** carried into the VM:
  `private_key_path` in config.toml names a file outside the data dir,
  and only the TOML text crosses the boundary, not referenced files.
  The in-VM worker (`--containerd-only`) never constructs a GitHub
  client, so the path string sits inert.

## What the in-VM daemon actually reads

The worker-mode code path dereferences a narrow slice of the config:

- `[dind]` — `enabled`, `allow_privileged`, cache settings.
- `[runtime.rlimits]` — per-container nofile, etc.
- `[log]` — log level/format.

Everything else (`[github]`, `[runner.windows]`, `[metrics]`,
`[vm.linux]`, `[webhook]`, tunnels, repo lists) is parsed into the
in-memory config but never read in worker mode. Worker mode returns
before the metrics server, providers, scheduler, and VM-boot blocks in
`serve()`, so a host config with `[metrics] enabled = true` does NOT
start a second metrics listener inside the VM. Future changes to
worker mode should preserve that invariant — the host scrapes in-VM
container stats via the Dispatch stream (#87) precisely so the VM
needs no listener of its own.

## Fallback

When `config.toml` doesn't exist on the host (fresh install before
first write), `buildBootInitrd` skips the entry and the init script
sees no `/assets/config.toml` — the in-VM daemon runs on its compiled
defaults plus the kernel-cmdline `ephemerd.dind*` flags from #88,
which are retained for exactly this case. Once a config exists, the
TOML wins (the cmdline flags force the same values they always did,
and `--config` only adds settings the flags don't cover).

## Failure modes worth knowing

- **Host config unreadable** (ACL mishap): treated as missing —
  defaults + cmdline. The init banner logs `host_config=` empty.
- **Malformed TOML on the host**: the host daemon itself fails to start
  first (it parses the same file), so a broken config never reaches a
  running VM in practice.
- **Operator edits config.toml while the VM is running**: not picked up
  until the next VM boot. Restart the ephemerd service.
- **Secrets rotation**: same story — restart the service; the initrd
  tail is regenerated with the new file on every boot.

## Lessons recorded

- **Deploying a draft build to the only Linux CI host turns "VM won't
  boot" into "CI is silently down."** The only symptom was a DEBUG-level
  skip log. Follow-up worth doing: a WARN (or health-endpoint signal)
  when Linux-labeled jobs are queued but the Linux dispatcher is
  unavailable.
- **HCS document changes need a boot test before deploy.** `0xc0370110`
  arrives at start time, not at document-build time; nothing in `mage
  ci` exercises it. A future smoke target that creates + starts a
  minimal VM would catch this class.

## File pointers

- Tail construction: `pkg/vm/initrd_windows.go` (`buildBootInitrd`)
- Call site + config path resolution: `pkg/vm/linuxvm_windows.go`
- VM-side staging: init script in `mage/download/download.go`
- Field: `vm.LinuxVMConfig.HostDataDir` in `pkg/vm/vm.go`
