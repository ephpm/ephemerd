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
   Longest-prefix wins so a child mount (`/etc/hosts`) is preferred over
   a parent (`/etc`).
2. **Upperdir match.** If `<upperdir>/X` exists, B's bind source is that
   path. Returned `rw` — A's upperdir is writable, and the GHA `_temp`
   case requires the sibling to read the next step's script written
   *after* `docker create`, so the directory mount must stay live.
3. **Lowerdir match.** If `/X` only exists in an image layer, B's bind
   source is that lowerdir path but the mount is forced `ro`. The
   lowerdir is shared with every other container using the same base
   image; a rw mount on top of it would corrupt the cache for unrelated
   jobs.
4. **No match → error.** Surfaced as HTTP 400 from
   `handleContainerCreate`. The pre-fix behavior was to silently drop;
   the new behavior fails loudly so the user sees a clear "bind mount
   /X -> /Y rejected" instead of a downstream "cannot open".

`filepath.Clean`/`path.Clean` normalizes `..` before the join, so a
malicious `/home/runner/../../etc/shadow` resolves to `/etc/shadow` and
either falls into A's rootfs (which means the sibling sees A's own
`/etc/shadow` — exactly what A could already see) or fails to resolve at
all. There is no source path that escapes the runner's rootfs envelope.

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

All tests pass with `CGO_ENABLED=0 go test ./pkg/dind/` and don't
require a real containerd.

## Deferred follow-ups

- **Windows-native `container:`.** Different snapshotter and mount
  semantics; needs its own translation layer or a clean "not supported"
  rejection at request time.
- **Symlink hardening.** `filepath.Clean` handles `..` but not symlinks
  that resolve outside the runner rootfs. The current upperdir/lowerdir
  walk only honors paths that exist as plain files/dirs within the
  layers, so we don't currently *open* the door — but if a future
  layer walk were to add `filepath.EvalSymlinks`, the call needs an
  after-the-fact prefix check to confirm the resolved path stays inside
  the snapshot directory.
- **Resolved-path caching.** Each `buildBindMounts` call queries the
  snapshotter and `os.Stat`s every source. A given runner doesn't
  change its layers within a job, so the resolution can be cached
  per-runner. Not worth optimizing until we see it in a profile.
