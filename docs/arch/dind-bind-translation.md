# Dind Bind Mount Translation

> **Status: implemented in PR `fix/dind-bind-mount-translate`.** Covers the v1
> shape and the deliberately-deferred follow-ups.

## Problem

GitHub Actions workflows can request a per-job container with the
`container:` directive:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
    container: ghcr.io/your-org/ci-image:latest
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

When this runs on an ephemerd self-hosted runner, the upstream GHA runner
binary handles `container:` by:

1. `docker pull` the requested image.
2. `docker create` a sibling container with a long bind list:
   ```
   -v /home/runner/_work:/__w
   -v /home/runner/_work/_temp:/__w/_temp
   -v /home/runner/_work/_actions:/__w/_actions
   -v /home/runner/_work/_tool:/__w/_tool
   -v /home/runner/_work/_temp/_github_home:/github/home
   -v /home/runner/_work/_temp/_github_workflow:/github/workflow
   -v /home/runner/externals:/__e:ro
   -v /var/run/docker.sock:/var/run/docker.sock
   ```
3. `docker start`, then for each step: write the wrapper script to
   `/home/runner/_work/_temp/<uuid>.sh` and `docker exec sh -e
   /__w/_temp/<uuid>.sh`.

Before this PR, ephemerd's dind shim accepted every `-v` in the API request
but silently dropped any bind whose source did not `os.Stat` on the dind
daemon's filesystem. Because the source paths live inside the runner
container's mount namespace, the dind daemon (running outside that namespace)
saw none of them. Every bind was dropped. The sibling started fine, but the
step's first `docker exec` failed with `sh: 0: cannot open
/__w/_temp/<uuid>.sh: No such file` — confusing because the wrapper script
exists, just not where the sibling looked.

This breaks every workflow that uses `container:`. Anthropic-style
workflows, ephpm, and most projects that want a reproducible toolchain
without baking a custom runner image depend on it.

## Two-container model

The fix has to acknowledge that at the moment of `docker create`, two
containers exist:

- **A: the runner container.** Created by `pkg/runtime`, lives in
  containerd namespace `"ephemerd"`, owns snapshot `<runnerID>-snapshot`
  on the `overlayfs` snapshotter. Inside A, the GHA runner binary writes
  workspace files under `/home/runner/_work/...` (upperdir), and reads
  pre-baked tools under `/home/runner/externals/...` (lowerdir).
- **B: the sibling about to be created** by the dind shim. Lives in
  `s.jobNamespace` (`"ephemerd-dind-<JobID>"`), will get its own
  snapshot.

A's filesystem decomposes into two distinct categories that need
different handling:

1. **Overlayfs rootfs:** upperdir (mutable, where the runner's writes
   land) plus lowerdirs (immutable image layers). All real paths on the
   dind daemon's filesystem, discoverable from
   `snapshotter.Mounts(ctx, "<runnerID>-snapshot")`.
2. **Special binds ephemerd installed into A:**
   - `/var/run/docker.sock` → `<DataDir>/jobs/<id>/docker/d.sock` (the
     dind socket file).
   - `/etc/hosts`, `/etc/resolv.conf` → per-runner config files written
     by `withHostsMount` / `withDNSMount`.
   - `r.cfg.RunnerMount` (e.g. `/home/runner/runner`) → `jobRunnerDir`
     (the per-job copy of the embedded runner directory, used on Windows
     and on custom images).

   These mounts are not in A's snapshot — they're explicit `Type:bind`
   entries in A's OCI spec.

When B asks for `-v /X:/Y`, `/X` is a path in A's mount namespace. To
hand the right thing to containerd as B's bind source, the dind shim has
to translate `/X` to wherever it actually lives on the dind daemon's
filesystem.

## Resolution policy

`pkg/dind/bindtranslate.go::translateBindSource` resolves in this order:

1. **Longest-prefix match against A's bind table.** If `/X` is under a
   destination ephemerd installed into A (e.g. `/var/run/docker.sock`),
   use the corresponding host source. The leftover suffix is appended.
   Longest-prefix wins so a child mount (`/etc/hosts`) is preferred
   over a parent (`/etc`).
2. **`<runner-rootfs-path>/X`.** When the runner's rootfs mount path is
   registered, sources resolve to a regular directory in the host's
   mount namespace — the same place runc mounted the runner's merged
   overlay (typically
   `/run/containerd/io.containerd.runtime.v2.task/<ns>/<id>/rootfs`).
   The runtime obtains the path via `os.Readlink("/proc/<pid>/root")`
   after the runner's task starts; that readlink returns the bundle
   rootfs as a host path. Binds from it see every layer's content
   because they go through the kernel's active overlay mount. Returned
   `rw`; writes copy-up into A's upperdir, which is A's own writable
   layer (no cross-job leak, no image-cache corruption).
3. **Upperdir match (fallback).** If no rootfs path is registered
   (test path), walk A's snapshot upperdir directly. Returned `rw`.
4. **Lowerdir match (fallback).** Walk A's snapshot lowerdirs.
   Returned `ro` — sibling writes through the bind would land on a
   shared image layer.
5. **No match → error.** Surfaced as HTTP 400 from
   `handleContainerCreate`. Pre-fix behavior was to silently drop; the
   new behavior fails loudly so the user sees "bind mount /X -> /Y
   rejected" instead of a downstream "cannot open".

**Why the bundle rootfs path beats the alternatives.** Two earlier cuts
of this fix didn't survive contact with production:

- *Per-layer walk* (returned the first lowerdir holding the requested
  path) broke for paths whose contents span multiple image layers. In
  the GHA `actions/runner` image, layer 4 creates the empty directory
  entry `/home/runner/externals/`, layer 22 adds `node20/bin/node` deep
  inside it. The walk picked layer 4, bound an empty tree, and
  `actions/checkout` failed downstream with `exec:
  "/__e/node20/bin/node": no such file`.
- *Procfs root link* (using `/proc/<pid>/root/X` directly as the bind
  source) readlinks correctly but the kernel refuses to use it for
  `mount(2)`: bind sources must resolve within the *calling* process's
  mount namespace, and `/proc/<pid>/root` walks into another. The
  kernel returns EINVAL.

The bundle rootfs path threads the needle: it's a normal host path the
kernel accepts as a bind source, AND it points at the kernel's active
overlay mount so every layer's content is visible.

`path.Clean` normalizes `..` before the join, so a
malicious `/home/runner/../../etc/shadow` resolves to `/etc/shadow` and
either falls into A's rootfs (which means the sibling sees A's own
`/etc/shadow` — exactly what A could already see) or fails to resolve at
all.

Lexical cleaning is not sufficient on its own — see the next section.

## Resolution and staging (issue #125)

The original design resolved a source to a *path string* and put that string
in B's OCI spec. runc then walked the string again, in its own process, at
task start. The job owns every byte of A's rootfs, so it could swap a
validated directory for a symlink in between and runc would mount the
symlink's target. Reproduced against real runc 1.3.4: the container received
the swapped target, exit status 0, nothing logged anywhere.

Two things fix it, and both are necessary.

**1. Contained resolution, once, to a descriptor**
(`pkg/dind/bindpin_linux.go`). Every source with a job-supplied component is
resolved with `openat2(2)` under `RESOLVE_IN_ROOT | RESOLVE_NO_MAGICLINKS`,
anchored at the branch's root (A's rootfs, or the host source from A's bind
table). The kernel enforces containment during the walk instead of a string
comparison afterwards, and the result is held open as an `O_PATH` descriptor,
so renaming or replacing a component afterwards cannot change what it points
at. Pre-5.6 kernels fall back to an `O_PATH|O_NOFOLLOW`
component-by-component walk that holds every directory on the way down open,
so `..` steps back to a descriptor already held rather than re-opening a
parent by name. `TestResolveBeneathWalk_MatchesOpenat2` asserts the two
resolvers land on the same inode — including for `..` arriving from a symlink
target, which is the case that made an earlier version of the walk stricter
than `openat2` while claiming equivalence. The fleet is all 6.x, so the walk
is reached only if the latch below trips.

The "no openat2" latch is process-global and permanent, so only `ENOSYS` and
`E2BIG` set it — errors that can only mean the kernel lacks the syscall. A
refusal (`EPERM`, e.g. a seccomp filter) falls back for that one call without
latching, and `EACCES`/`EINVAL` are ordinary failures that fail the bind
closed. Whichever path it takes, the first bind afterwards logs a WARN naming
the reason; a node silently resolving binds by the fallback used to be
indistinguishable from one that was not.

`RESOLVE_IN_ROOT` rather than Go's `os.Root`: an absolute symlink is
*reinterpreted* relative to the root, which is what a container rootfs means
and what merged-usr images (`/bin -> /usr/bin`) require. `os.Root` rejects
absolute symlinks outright and would break every Ubuntu runner image. Note the
consequence: a symlink inside A's rootfs whose target is the *host* path of
that rootfs no longer resolves. That cannot occur in a container image —
nothing inside a container can name the host path of its own rootfs — and
following it would mean resolving against the host's root, which is the escape.

**2. Staging, because a descriptor cannot go in the spec**
(`pkg/dind/bindstage_linux.go`). The obvious handoff — putting
`/proc/<ephemerd-pid>/fd/<n>` in `ocispec.Mount.Source` — does not work. runc
does not re-resolve the string (its own error echoes it verbatim), but
`mount(2)` rejects it:

```
error mounting "/proc/3543706/fd/3" to rootfs at "/marker":
mount src=/proc/3543706/fd/3, dst=/marker, dstFd=/proc/thread-self/fd/11,
flags=MS_BIND|MS_REC: invalid argument
```

A bind source must live in the *caller's* mount namespace, and runc always has
its own. The rejection is unconditional — legitimate binds fail identically —
so an fd handoff is a functional regression, not a fix. (This is recorded as
an executable test, `TestBindStaging_RealRunc_ProcFdSourceIsRejected`, so the
next person does not rediscover it from an outage.)

Instead ephemerd performs the bind itself, in its own mount namespace where
`/proc/self/fd/<n>` resolves fine, onto a path it owns:

```
openat2(RESOLVE_IN_ROOT|RESOLVE_NO_MAGICLINKS)          -> pinned fd
mount("/proc/self/fd/N" -> <data>/dind-binds/<job>/<n>)  (same ns: works)
spec.Source = <data>/dind-binds/<job>/<n>
```

runc re-walks the staging path, which is fine: nothing in the name is derived
from the job's request (the leaf is a bare counter), and no component of it is
swappable by anyone but root. `ensureTrustedAncestry` checks that precondition
on first use and fails the bind rather than assuming it — every component must
be a real directory (not a symlink) owned by root or by ephemerd's own euid,
and not group- or other-writable unless it carries the sticky bit, which is
what makes a data dir under `/tmp` acceptable while a plain group-writable one
is not. The staging directories ephemerd creates itself are 0700.

Sources with no job-supplied
component — `/var/run/docker.sock`, `/etc/hosts`, `/etc/resolv.conf`, the
runner-mount root itself — are still passed through unpinned and unstaged;
their paths are entirely ephemerd's.

**Staging directory location.** `<data>/dind-binds/<job-id>/`, deliberately
*not* under `<data>/jobs/<job-id>/`, which the runtime's orphan sweep
`os.RemoveAll`s. Recursively deleting a directory containing a live bind mount
deletes the files visible *through* the mount — here, A's own rootfs.

**Lifecycle.** Pins are held on the `containerEntry` for the container's whole
life (not just until the task starts: `docker restart` makes runc re-read the
spec) and released in `cleanupContainer`. `Server.Stop` tears down the job's
whole staging directory as a backstop. A hard kill skips both, so
`dind.SweepStagedBinds` runs at daemon startup, from `Runtime.CleanOrphans`,
*before* the container and snapshot sweep — a leaked staging mount holds a
reference to the rootfs it was bound from, which makes the snapshot
undeletable.

**Failure mode inverted, on purpose.** Before, a swapped source mounted
silently. Now a source that cannot be staged fails the `docker create` with a
400. That is the correct direction, but it is a new way for a job to break, so
the error names the staging directory and the requirement (root with
`CAP_SYS_ADMIN`, writable data dir).

**Operator note.** `ensureTrustedAncestry` is the one new way an otherwise
working deployment can start rejecting every bind: an ephemerd whose
`--data-dir` sits under a group- or world-writable directory without the
sticky bit now fails `docker create` instead of staging into a path someone
else could swap. The default `/var/lib/ephemerd` is fine, and so is anything
under `/tmp` (sticky). A custom data dir on a shared or relaxed-permission
mount is not, and the error says which component and why.

**Windows and macOS are unaffected.** Bind translation is not wired into
either native container path; `bindpin_other.go` / `bindstage_other.go` exist
only so the translation policy tests build and run on a dev host, and they
carry no security property.

## Security envelope

The sibling B can only see what A could already see. There is no
privilege expansion:

- Bind table entries point at host paths that ephemerd itself installed
  into A. B reaches what A reached, no more.
- Upperdir / lowerdir entries point at A's snapshot. B mounts a path
  inside A's rootfs; A already had access to that same content.
- Anything not in the bind table or A's snapshot is rejected. There is
  no code path that takes an attacker-supplied `/etc/shadow` or `/` and
  hands it to containerd — the silent-drop bug accidentally provided
  this property and the loud-fail fix preserves it explicitly.

The high-risk anti-pattern this design avoids is the standard
"mount the Docker socket into a runner" model, where the dind sees real
host paths and arbitrary `-v` sources are honored. That model is
well-known to be root-on-host. Ours is not, because dind never resolves
sibling sources against the host filesystem directly.

## Lifecycle

`pkg/runtime.Destroy` cleans up in this order:

1. Kill the runner task.
2. Delete the task.
3. Teardown networking.
4. **`env.Dind.Stop()`** — calls `destroyAllContainers()`, which kills
   every sibling and deletes them, then drops the per-job containerd
   namespace.
5. **`container.Delete(WithSnapshotCleanup)`** — removes A's container
   and its snapshot (the upperdir disappears from disk).

Step 4 runs before step 5, so siblings are gone before A's upperdir is
removed. A sibling cannot end up with a stale bind in normal teardown.
If step 4 fails to fully clean a sibling (containerd wedged, kill
timeout), step 5 still proceeds and the zombie sibling's mount becomes
stale — but since the sibling's task is already killed at that point,
nothing tries to use the stale mount and the leak is bounded to whatever
namespace-cleanup pass eventually reaps it.

No snapshot lease extension is needed. The earlier draft of this
design proposed leases for siblings outliving the runner; that scenario
doesn't exist in ephemerd's job model.

## Wiring

`pkg/runtime/runtime.go`, right after `r.client.NewContainer(...)` succeeds
and before `task.Start(...)`:

```go
if dindServer != nil && goruntime.GOOS != "windows" {
    bindMappings := map[string]string{}
    if dindServer.SocketPath() != "" {
        bindMappings["/var/run/docker.sock"] = dindServer.SocketPath()
    }
    hostDataDir := filepath.Dir(r.cfg.LogDir)
    bindMappings["/etc/hosts"] = filepath.Join(hostDataDir, "hosts", id+".hosts")
    bindMappings["/etc/resolv.conf"] = filepath.Join(hostDataDir, "dns", id+".conf")
    if jobRunnerDir != "" && r.cfg.RunnerMount != "" {
        bindMappings[r.cfg.RunnerMount] = jobRunnerDir
    }
    dindServer.SetRunnerRootfs(snapshotName, bindMappings)
}
```

`pkg/dind/dind.go::SetRunnerRootfs` stores the snapshot key and a copy of
the bind table on the Server. `pkg/dind/containers.go::buildBindMounts`
consults them when translating each `-v` from
`req.HostConfig.Binds`. The translation runs *before* the OCI spec is
finalized, so a rejection turns into HTTP 400 cleanly.

## Windows

The `goruntime.GOOS != "windows"` guard skips the registration only on
the Windows-native runner code path. There are two scenarios:

- **Linux job on a Windows host.** ephemerd's host daemon spawns a
  Hyper-V Linux VM (`pkg/vm/linuxvm_windows.go`); the runner container
  inside is created by a *separate* ephemerd process running as Linux
  inside the VM. That in-VM process sees `goruntime.GOOS == "linux"`,
  so its `pkg/runtime.Create()` registers the rootfs normally. The
  translation works.
- **Windows-native job on a Windows host.** Hyper-V isolated Windows
  container, `windowsfilter` snapshotter. There is no overlay
  upperdir/lowerdir to walk, and Windows bind semantics differ
  (`Mount.Type` is empty rather than `"bind"`, `Options` uses different
  flags, junctions instead of `rbind`). Translation needs a separate
  design. The GHA `container:` directive for `runs-on: windows-*` is
  unusual enough that this is deferred.

## Tests

`pkg/dind/bindtranslate_test.go`:

- **9 pure-function tests** for `translateBindSource`: upperdir match
  returns rw, lowerdir match forces ro, runner-bind translation
  including subpath, longest-prefix wins over parent, unknown source
  rejection, relative-path rejection, `..` traversal stays bounded,
  upper-over-lower preference (overlay copy-up semantics).
- **3 integration tests** for `Server.buildBindMounts`: the full
  8-bind set from a real GHA `container:` failure log (asserts
  docker.sock translation, `_temp` lands in upperdir rw, `externals`
  lands in lowerdir ro), unknown source surfaces a 400-shaped error,
  no-rootfs-registered rejects rather than silently allowing.

All of the above pass with `CGO_ENABLED=0 go test ./pkg/dind/` and don't
require a real containerd.

`pkg/dind/bindstage_linux_test.go` (Linux, **root only** — `mount(2)` needs
`CAP_SYS_ADMIN` and the project's Linux CI runner is unprivileged) covers the
staging layer itself: the swap-after-validation escape, a control proving the
pre-fix shape *does* follow the swap, the legitimate-bind regression set
(directory, regular file, auto-mkdir, merged-usr symlink traversal, and the
three passthrough binds), teardown, the startup sweep, and the trusted-ancestry
precondition.

`pkg/dind/bindstage_runc_linux_test.go` (Linux, root, and
`EPHEMERD_TEST_RUNC=<path to runc>`) is the end-to-end proof, because nothing
on the daemon side can observe what runc's `mount(2)` resolves:

```
sudo EPHEMERD_TEST_RUNC=$PWD/pkg/containerd/embed/runc \
  go test ./pkg/dind/ -run TestBindStaging_RealRunc -v
```

Against runc 1.3.4 it reports the control leaking (`CONTAINER-SAW: SWAPPED`),
the staged source holding (`CONTAINER-SAW: PINNED`), and the `/proc/<pid>/fd`
source being rejected with `invalid argument`.

**A green `go test ./pkg/dind/` on an unprivileged host proves none of this.**
The tests that carry the security property skip there, loudly. The previous
attempt at #125 shipped a spec runc rejects outright and the package was still
`ok`.

## Deferred follow-ups

- **Windows-native `container:`.** Different snapshotter and mount
  semantics; needs its own translation layer or a clean "not supported"
  rejection at request time.
- ~~**Symlink hardening.**~~ Done, and then done again properly: the
  after-the-fact prefix check this bullet proposed was implemented, and was
  the bug — see "Resolution and staging (issue #125)" above. Containment is
  now enforced by the kernel during a single resolution, and the result is a
  descriptor rather than a name.
- **`open_tree(OPEN_TREE_CLONE)` + `move_mount`.** A cleaner way to hand a
  detached mount to the runtime than a staging path, and it would remove the
  staging directory and its lifecycle entirely. It needs the runtime to accept
  a mount fd, which the OCI spec does not express and containerd does not
  plumb, so it is not reachable from here today.
- **Resolved-path caching.** Each `buildBindMounts` call queries the
  snapshotter and `os.Stat`s every source. A given runner doesn't
  change its layers within a job, so the resolution can be cached
  per-runner. Not worth optimizing until we see it in a profile.
