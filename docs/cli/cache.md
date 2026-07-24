---
title: cache
weight: 10
---

Inspect and clear ephemerd's on-disk caches. Every cache ephemerd manages
lives under the data directory (`/var/lib/ephemerd` on Linux/macOS,
`C:\ProgramData\ephemerd` on Windows; override with `--data-dir` or
`EPHEMERD_DATA_DIR`).

```
ephemerd cache list [--json]
ephemerd cache clear <name> [--yes]
ephemerd cache clear --all [--yes]
```

## Caches

| Name | Path (relative to data dir) | Description | Clearable while running |
|------|-----------------------------|-------------|-------------------------|
| `images` | `images/` | Staged OCI image tarballs imported into containerd on startup | yes |
| `gomod` | `cache/gomod/` | Go module proxy cache (GOPROXY) served to job containers | yes |
| `buildkit` | `buildkit/` | Embedded BuildKit solver cache + history (`docker build` layers) | no |
| `worker` | `worker/` | BuildKit worker snapshot/content root | no |
| `runners` | `runners/` | Extracted GitHub Actions runner binaries (per version/OS) | no |
| `cni` | `cni/` | Extracted CNI plugin binaries (per version) | no |
| `artifacts` | `artifacts/` | OCI artifact layers extracted for macOS VM jobs (per job) | yes |
| `vm` | `vm/` | VM images and per-job clones (macOS `base.img`, Linux VM, run dirs); the `embed/` assets are preserved | no |
| `containerd` | `containerd/` | containerd content store, snapshots, and state (backs all images) | no |
| `jobs` | `jobs/` | Per-job runner workdirs (`_work`, extracted php-sdk, dind sockets); in-flight job dirs are skipped | yes |

Per-job runner workdirs persist under `jobs/<job-id>/` rather than being
destroyed the instant a job ends (on Windows they were in fact leaking — see
the note below), so a stale extraction such as a bad `php-sdk` can block a
subsequent build. `cache clear jobs` clears them. It is safe to run while the
daemon is up: the command asks the daemon for its currently running jobs (over
the control socket) and skips those directories, removing only the leftovers.
If the daemon is not running, nothing is in flight and every `jobs/*` dir is
removed.

> The underlying Windows leak — job workdirs that were never removed on job
> completion — is fixed separately (per-job cleanup on completion plus a
> startup sweep of orphaned dirs). `cache clear jobs` is the operator-facing
> escape hatch for clearing any that predate that fix or survive a crash.

The per-repo **dind image cache** is not in this list because it lives in
containerd metadata namespaces (`ephemerd-dind-cache-*`), not a directory. It
is pruned automatically by the running daemon on the `cache_prune_interval`
schedule (see [configuration](../getting-started/configuration/)) and is
surfaced by `cache list` only as a note.

## List caches

```bash
$ ephemerd cache list
Data dir: /var/lib/ephemerd

NAME         SIZE       CLEARABLE  PATH
images       2.4 GB     live       /var/lib/ephemerd/images
gomod        512.0 MB   live       /var/lib/ephemerd/cache/gomod
buildkit     8.1 GB     stopped    /var/lib/ephemerd/buildkit
...
TOTAL        14.2 GB
```

Pass `--json` for machine-readable output (data dir, per-cache sizes in bytes,
and the total) suitable for scripting.

## Clear caches

Clear a single cache by name:

```bash
ephemerd cache clear gomod
```

Clear everything that is safe to clear:

```bash
ephemerd cache clear --all
```

Destructive clears prompt for confirmation. Pass `--yes` (aliases `--force`,
`-y`) to skip the prompt in scripts.

### Safety

- **Data-root guard.** Every target path is resolved against the data dir and
  refused if it escapes it. The command can only ever delete inside the known
  cache roots — never the data-dir root itself, and never an arbitrary path.
- **Running-daemon guard.** Caches marked `stopped` above back live daemon
  state (containerd, BuildKit, in-flight VMs). While a daemon is running they
  are skipped by `--all` and refused for a named clear, unless you pass
  `--force`. Stop the daemon first for a clean clear.
