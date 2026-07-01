# Native macOS Runner

> **Status: implemented.** See `pkg/native/`. Notable deviations from this
> proposal, discovered during implementation:
>
> - **Privilege dropping**: jobs run as a hidden `_ephemerd` service user
>   (created lazily, like `_www`), not as the daemon's root user. Per-job
>   ephemeral users were attempted but abandoned: macOS user *deletion*
>   via dscl/sysadminctl requires Full Disk Access and wedges
>   opendirectoryd without it, while creation works fine.
> - **Sandbox network rules**: `sandbox-exec` does not support CIDR
>   notation (`10.0.0.0/8`). The profile blocks localhost outbound and all
>   port binding; RFC1918 blocking needs pf firewall rules (follow-up).
> - **DEVELOPER_DIR** is resolved via `xcode-select -p` instead of
>   hardcoding the Xcode.app path (hosts with only CLT broke otherwise).
> - **Runner extraction** is OS-suffixed (`runners/<ver>-<goos>`) so the
>   macOS host and Linux VM don't collide on the shared data dir.

## Problem

macOS jobs currently run in per-job Virtualization.framework VMs (APFS
clone-on-write from a base image). This works but has hard limits:

- Apple restricts macOS VMs to **2 concurrent instances** per host.
- Each VM needs **4 GB+ RAM** (2 GB absolute minimum, unusable in practice).
- An 8 GB Mac mini can run at most **2 concurrent macOS jobs**.
- VM boot adds **10-15 seconds** of overhead per job.

For repos that don't need VM-level isolation (trusted internal CI, Xcode
builds, Go tests), a native execution mode that runs the GitHub Actions
runner directly on the host would allow **4-6+ concurrent jobs** on the
same hardware with zero boot overhead.

## Proposal

Add a **native** macOS execution mode alongside the existing VM mode.
The mode is configured per-repo. The VM path is untouched -- this is
purely additive.

## Config design

A new `[runner.macos]` section controls macOS job routing. It lives under
`[runner]` (not `[vm.macos]`) because native jobs don't involve VMs.

```toml
[runner.macos]
mode = "vm"         # default mode: "vm" or "native"
max_native = 4      # max concurrent native jobs (no Apple limit applies)

# Per-repo overrides. Repo name matches github.repos entries.
[runner.macos.repos]
php-sdk = "native"
ephemerd = "native"
# Repos not listed here inherit the top-level mode.
```

Config struct additions in `pkg/config/config.go`:

```go
type RunnerConfig struct {
    // ... existing fields ...
    MacOS MacOSRunnerConfig `toml:"macos"`
}

type MacOSRunnerConfig struct {
    Mode      string            `toml:"mode"`       // "vm" (default) or "native"
    MaxNative int               `toml:"max_native"` // max concurrent native jobs (default 4)
    Repos     map[string]string `toml:"repos"`      // repo -> "vm" or "native"
}
```

`MacOSRunnerConfig.ModeForRepo(repo)` returns `"native"` or `"vm"` by
checking the per-repo map first, then falling back to the top-level mode,
then defaulting to `"vm"`.

### Why not extend `[runner.images]`?

`[runner.images]` maps repos to OCI container images. Native macOS jobs
don't use container images at all -- they run directly on the host. Mixing
these two concepts in the same config block would be confusing.

## Scheduler flow

`handleQueued` already routes macOS jobs to `handleMacOSJob`. The change
adds a branch at the top of `handleMacOSJob`:

```
handleQueued
  └─ isMacOSJob?
       └─ handleMacOSJob
            ├─ ModeForRepo == "native" → handleNativeMacOSJob (new)
            │   └─ acquire nativeMacSem (max_native)
            └─ ModeForRepo == "vm" → existing VM path
                └─ acquire macSem (max 2)
```

A new semaphore `nativeMacSem` (capacity = `max_native`) is separate from
the existing `macSem` (VM concurrency, capped at 2 by Apple). This means
a host can run 2 VM jobs + 4 native jobs simultaneously if both modes are
in use.

The `canHandleJob` check for `"macos"` labels also needs updating:
currently it requires `MacOSVMConfig != nil`. With native mode, macOS jobs
are handleable on darwin hosts even without a VM disk image, as long as
the runner config allows native mode for that repo.

## Native runner lifecycle

New package: `pkg/native/native_darwin.go` (build-tagged `darwin`).

### 1. Create workspace

```
<data_dir>/native/<job_id>/
  ├── home/          → $HOME for the job
  ├── tmp/           → $TMPDIR for the job
  ├── work/          → runner _work directory
  └── runner/        → per-job copy of the GHA runner binary
```

The runner is extracted from the embedded `pkg/runner` tarball into the
per-job directory. This is the same runner binary used by the VM path,
just extracted to a different location.

### 2. Set up environment

```go
env := []string{
    "HOME=" + jobHome,
    "TMPDIR=" + jobTmp,
    "RUNNER_WORK_FOLDER=" + jobWork,
    "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
    // Xcode: use host's installation
    "DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer",
}
```

Host tooling (`/opt/homebrew`, `/Applications/Xcode.app`, `/usr/local`)
is shared read-only by virtue of the OS -- no bind mounts needed. Each
job just gets its own HOME/TMPDIR/work directory so outputs don't collide.

### 3. Start runner

```go
cmd := exec.CommandContext(ctx, "./run.sh", "--jitconfig", jitConfig)
cmd.Dir = runnerDir
cmd.Env = env
cmd.Stdout = logFile
cmd.Stderr = logFile
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group
err := cmd.Start()
```

`Setpgid: true` puts the runner and all its children in a new process
group so we can `kill(-pgid, SIGTERM)` on cleanup.

### 4. Wait for exit

Block on `cmd.Wait()`. Return the exit code.

### 5. Cleanup

1. Kill the process group (`syscall.Kill(-pgid, SIGKILL)`) if still alive.
2. `pkill -9 -P <pid>` as a fallback for any orphaned children.
3. `os.RemoveAll(jobDir)` to delete the workspace.
4. Deregister the runner from the provider.

## Isolation model

| Layer | Native | VM |
|-------|--------|----|
| Filesystem | Per-job HOME/TMPDIR/workdir + sandbox deny on sensitive paths | Full disk clone |
| Processes | Process group (`setpgid`), killed on cleanup | Separate kernel |
| Network | Sandbox: deny RFC1918/localhost outbound + deny port binding | NAT with firewall |
| Users | Shared macOS user | Isolated user per VM |
| Secrets | Sandbox denies read on key paths, env cleared on exit | VM memory destroyed |

### Sandbox profile (required for native mode)

Every native job runs under `sandbox-exec -f <profile>`. The sandbox
is **inherited by all child processes** and **enforced by the kernel**.
No process can escape it without root.

The profile is generated per-job (to include the job-specific directory
paths) and written to the job workspace:

```scheme
(version 1)
(allow default)

;; === Network isolation ===

;; Block outbound to private networks
(deny network-outbound (remote ip "localhost:*"))
(deny network-outbound (remote ip "10.0.0.0/8:*"))
(deny network-outbound (remote ip "172.16.0.0/12:*"))
(deny network-outbound (remote ip "192.168.0.0/16:*"))
(deny network-outbound (remote ip "169.254.0.0/16:*"))

;; Block binding to any port — prevents jobs from running servers
;; that other jobs could connect to. This closes the inter-job
;; localhost attack vector entirely.
(deny network-bind (local ip "*:*"))

;; Allow DNS (required for public internet access)
(allow network-outbound (remote udp "*:53"))
(allow network-outbound (remote tcp "*:53"))

;; === Filesystem isolation ===

;; Block sensitive host paths
(deny file-read* (subpath "/Users/luthermonson/.ssh"))
(deny file-read* (subpath "<data_dir>/config.toml"))
(deny file-read* (literal "<data_dir>/ephemerd.sock"))
(deny file-read* (subpath "<data_dir>/vm"))

;; Block writes to shared tools (read-only access only)
(deny file-write* (subpath "/opt/homebrew"))
(deny file-write* (subpath "/Applications"))
(deny file-write* (subpath "/usr/local"))

;; Allow writes to the job directory only
(allow file-write* (subpath "<job_dir>"))
(allow file-write* (subpath "/private/tmp"))
```

In Go, the runner is launched as:

```go
cmd := exec.CommandContext(ctx, "sandbox-exec", "-f", profilePath,
    "./run.sh", "--jitconfig", jitConfig)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
```

### What this provides

- **Network isolation**: jobs cannot reach the LAN, other machines, or
  the ephemerd control socket. Jobs cannot bind ports, so they cannot
  communicate with each other via localhost.
- **DNS allowed**: jobs can resolve public hostnames and connect to
  public internet (GitHub, package registries, etc.).
- **Filesystem write isolation**: jobs can only write to their own
  workspace. Shared tools (`/opt/homebrew`, `/Applications`) are
  read-only. Sensitive host files (SSH keys, config, VM assets) are
  blocked entirely.
- **Process isolation**: `setpgid` + process group kill ensures no
  orphaned processes survive between jobs.
- **Environment isolation**: each runner process gets a controlled set
  of environment variables. No leakage from the daemon process.

### Remaining limitations (accepted for trusted repos)

- **No per-job user isolation.** All jobs run as the same macOS user.
  A job can `ps aux` and see other jobs' PIDs (but not interact with
  them — the sandbox blocks sensitive files and network).
- **No resource limits.** macOS has no cgroups. A runaway build can
  starve other jobs of CPU/memory. Mitigated with `nice` (CPU priority)
  and `ulimit` (memory soft limit) on the runner process.
- **Read access to non-denied paths.** Jobs can read world-readable
  files outside the deny list. The sandbox profile should be kept
  up-to-date with any new sensitive paths.

## Comparison table

| Dimension | Native | VM |
|-----------|--------|----|
| Boot time | ~0s (fork+exec) | 10-15s |
| Memory per job | ~200 MB (runner process) | 4+ GB |
| Max concurrent (8 GB mini) | 4-6 | 2 |
| Isolation | Process group + directory | Full VM (separate kernel) |
| Network isolation | None | NAT + firewall |
| Security | Trusted repos only | Untrusted OK |
| Xcode/Homebrew | Shared from host | Pre-installed in base image |
| Setup complexity | Low (just extract runner) | High (IPSW install, clone) |
| Apple VM limit | Not applicable | 2 per host |

## What changes

### `pkg/config/config.go`

Add `MacOSRunnerConfig` struct to `RunnerConfig`. Add `ModeForRepo(repo)`
method.

### `pkg/scheduler/scheduler.go`

- Add `nativeMacSem chan struct{}` field to `Scheduler`.
- Initialize from `cfg.Runner.MacOS.MaxNative` (default 4).
- Update `canHandleJob`: accept macOS jobs on darwin even without
  `MacOSVMConfig` when native mode is configured for the repo.
- Split `handleMacOSJob`: check `ModeForRepo` and route to
  `handleNativeMacOSJob` or the existing VM path.

### New: `pkg/native/native_darwin.go`

Native runner lifecycle:

```go
type Runner struct { /* workspace paths, cmd, pgid */ }

func New(dataDir string, jobID string, jitConfig string, log *slog.Logger) (*Runner, error)
func (r *Runner) Start(ctx context.Context) error
func (r *Runner) Wait(ctx context.Context) (int, error)
func (r *Runner) Stop()
```

A `native_other.go` stub returns errors on non-darwin platforms.

### `cmd/ephemerd/runtime_darwin.go`

Pass `cfg.Runner.MacOS` to the scheduler config so it can read per-repo
mode overrides.

## Decisions

### 1. Homebrew: per-job writable prefix over shared read-only base

Jobs need `brew install` for build deps, but we can't let one job's
installs pollute another. The solution uses Homebrew's relocatable
architecture:

**Host setup (one-time):** `/opt/homebrew` is pre-installed with common
tools (Go, mage, etc.) and marked read-only for the runner user.

**Per-job overlay:**

```
<data_dir>/native/<job-id>/
  └── homebrew/           → HOMEBREW_PREFIX, HOMEBREW_CELLAR, HOMEBREW_TEMP
      ├── Cellar/         → per-job installs land here
      ├── lib/
      ├── bin/            → symlinked from /opt/homebrew/bin at job start
      └── Homebrew/       → lightweight Homebrew checkout (or symlink)
```

Environment for the runner process:

```bash
HOMEBREW_PREFIX=<job_dir>/homebrew
HOMEBREW_CELLAR=<job_dir>/homebrew/Cellar
HOMEBREW_TEMP=<job_dir>/tmp
PATH=<job_dir>/homebrew/bin:/opt/homebrew/bin:/usr/local/bin:...
```

How it works:

1. At job start, create `<job_dir>/homebrew/bin` and symlink all
   executables from `/opt/homebrew/bin` into it. This gives the job
   read access to pre-installed tools.
2. Set `HOMEBREW_PREFIX` and `HOMEBREW_CELLAR` to the per-job dir.
   Any `brew install` writes to the job's Cellar, not the host's.
3. The job's `homebrew/bin` is first in PATH, so newly installed
   formulas shadow the host versions if there's a conflict.
4. At job end, `rm -rf <job_dir>` deletes everything — installs,
   caches, temp files.

**Why not a full Homebrew clone?** Cloning the Homebrew repo takes
~10 seconds and ~500 MB. Symlinking the host's existing install is
instant and zero-copy. The job only needs a writable prefix for new
installs.

**Why not just share `/opt/homebrew` read-write?** Jobs would step on
each other. One job upgrading a formula mid-build could break another
job. Per-job prefix keeps them independent.

### Host provisioning contract (build dependencies)

Native mode runs jobs **on the bare host** — no per-job VM, no container
image. This is the key operational difference from the VM path, and it
has a consequence that bit us in production:

> Any build dependency a workflow assumes is "already installed" must
> actually exist **on the host**.

In the VM path those deps lived in the macOS VM **base disk image**
(Tart-provisioned once: Xcode, Homebrew, `brew install llvm@17`, …). The
`runner-ci-macos-deps` OCI image only carried ephemerd's own Go tooling
(golangci-lint, mage, runner tarballs) as a VM overlay — it never held
the language/build toolchains. So there is **no image to unzip** onto a
native host: the deps only ever existed inside a full macOS disk image,
for which we have no portable manifest.

Concretely, ephpm's Rust build pins
`LIBCLANG_PATH=$(brew --prefix llvm@17)/lib` for bindgen. On the VM that
path existed (baked into base.img); on a fresh native host it did not,
and every `Build` job died with `Unable to find libclang`. The `Test`
(nextest) jobs passed because they don't invoke bindgen.

Two ways to satisfy the contract; a fleet should pick one:

1. **Provision the host** to match what workflows expect. Run
   `scripts/provision-native-macos.sh` on each native runner host and
   extend its formula list whenever a job fails on a missing dep. Simple,
   but host state must be kept in sync across the fleet.

2. **Workflows install their own deps** (the reproducible, host-agnostic
   option, and the one the per-job Homebrew overlay above was designed
   for): add `brew install llvm@17 …` to the macOS job. Installs land in
   the job's writable prefix and are discarded with the job. Preferred
   long-term, but it's a change in the *consuming* repo's workflow, not
   ephemerd.

Either way, native mode's dependency surface is the host, not an image —
that is the durable takeaway.

### 2. Keychain: per-job temporary keychain

Each native job gets its own temporary keychain:

```bash
KEYCHAIN=<job_dir>/keychain/job.keychain-db
security create-keychain -p "" "$KEYCHAIN"
security default-keychain -s "$KEYCHAIN"
security unlock-keychain -p "" "$KEYCHAIN"
```

At cleanup:

```bash
security delete-keychain "$KEYCHAIN"
```

This prevents jobs from accessing each other's signing identities and
avoids polluting the host login keychain. Jobs that need code signing
import their certs into the per-job keychain via `security import`
(standard GitHub Actions pattern — `apple-actions/import-codesign-certs`
does exactly this).

### 3. Concurrency: static config, default 4

`max_native = 4` is the default. Operators set it based on their
hardware. No auto-detection — the right value depends on workload
(CPU-heavy Xcode builds want fewer, lightweight Go tests want more).

The value only caps native macOS jobs. Linux jobs (in the VM) and
macOS VM jobs have their own separate limits.
