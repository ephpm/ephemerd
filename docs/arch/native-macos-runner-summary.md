# Native macOS Runner for ephemerd

## Problem

macOS jobs currently run in per-job Virtualization.framework VMs. This works but has hard limits:

- Apple restricts macOS VMs to **2 concurrent instances** per host
- Each VM needs **4+ GB RAM**
- VM boot adds **10-15 seconds** of overhead per job
- An 8 GB Mac mini can run at most **2 concurrent macOS jobs**

## Solution

A new **native** execution mode that runs the GitHub Actions runner directly on the host. For trusted repos that don't need VM-level isolation (internal CI, Xcode builds, Go tests), this enables:

- **4-6+ concurrent jobs** on the same hardware (configurable)
- **Zero boot overhead** — fork+exec, not VM boot
- **~200 MB per job** instead of 4+ GB

The VM path is untouched — this is purely additive. Mode is configured per-repo.

## Config

```toml
[runner.macos]
mode = "vm"         # default for repos not listed below
max_native = 4      # max concurrent native jobs

[runner.macos.repos]
"ephpm/*"           = "native"  # whole org runs native
"ephpm/secret-repo" = "vm"     # except this one (exact match wins over wildcard)
"someuser/ephemerd" = "vm"     # fork stays on VM
```

Resolution order: exact `org/repo` match > `org/*` wildcard > top-level mode > default `"vm"`.

## How it works

Each native job gets its own isolated workspace:

```
<data_dir>/native/<job_id>/
  ├── home/          → $HOME
  ├── tmp/           → $TMPDIR
  ├── work/          → runner _work directory
  ├── runner/        → per-job copy of the GHA runner binary
  ├── homebrew/      → per-job Homebrew prefix (symlinks to host /opt/homebrew)
  └── keychain/      → per-job macOS keychain
```

### Isolation layers

| Layer | How |
|-------|-----|
| Filesystem | Per-job HOME/TMPDIR/workdir. Sandbox blocks writes to `/opt/homebrew`, `/Applications`, `/usr/local`. Sensitive paths (SSH keys, ephemerd config, VM assets) blocked entirely. |
| Processes | `setpgid` puts runner + children in own process group. Killed via `kill(-pgid)` on cleanup. |
| Network | `sandbox-exec` blocks localhost outbound (prevents reaching ephemerd control socket or other jobs) and blocks port binding (prevents inter-job communication). DNS allowed. Public internet allowed. |
| Secrets | Per-job keychain created/destroyed. Environment cleared. |
| Homebrew | Host `/opt/homebrew` is read-only. Per-job prefix for `brew install` — installs are isolated and destroyed with the job. |

The runner is launched via macOS `sandbox-exec`, which is kernel-enforced and inherited by all child processes.

## Concurrency

A separate semaphore (`nativeMacSem`) gates native jobs independently from VM jobs (`macSem`). A host can run **2 VM jobs + 4 native jobs simultaneously** if both modes are in use.

## Scheduler flow

```
handleQueued
  └─ isMacOSJob?
       └─ ModeForRepo == "native" → handleNativeMacOSJob
       │   └─ acquire nativeMacSem
       │   └─ claimJob (register JIT runner with GitHub)
       │   └─ native.New → copy runner, generate sandbox, setup env
       │   └─ native.Start → sandbox-exec ./run.sh --jitconfig <jit>
       │   └─ native.Wait → block until job completes
       │   └─ native.Stop → kill process group, delete keychain, rm workspace
       │   └─ ReleaseJob (deregister runner)
       │
       └─ ModeForRepo == "vm" → handleMacOSJob (existing, unchanged)
           └─ acquire macSem
           └─ boot Virtualization.framework VM
```

## What's left

- **Private network blocking** (10.x, 172.16.x, 192.168.x): `sandbox-exec` doesn't support CIDR notation. Needs `pf` firewall rules — separate follow-up.
- **Resource limits**: macOS has no cgroups. A runaway build can starve others. Mitigated with `nice`/`ulimit` in a future iteration.
- **No per-job user isolation**: all jobs run as the same macOS user. Jobs can see each other's PIDs via `ps` but can't interact (sandbox blocks sensitive files and network).

## Comparison

| | Native | VM |
|--|--------|-----|
| Boot time | ~0s | 10-15s |
| Memory per job | ~200 MB | 4+ GB |
| Max concurrent (8 GB mini) | 4-6 | 2 |
| Isolation | Sandbox + process group | Full VM |
| Security | Trusted repos only | Untrusted OK |
| Apple VM limit | N/A | 2 per host |
