# Self-Healing Runner Version

> **Status: wired and ON BY DEFAULT since 0.3.0.** The engine from #220
> (`pkg/runnerversion`: signed manifest, verified fetch + content-addressed
> cache + atomic swap, zero-work detector + circuit breaker, official-image
> probe + override) is driven by `runnerversion.Controller` and assembled in
> `cmd/ephemerd/selfheal.go` for both the host scheduler daemon and the
> in-VM worker. A node with no `[runner.self_heal]` table gets all of it:
> boot-verify, zero-work detection + breaker, mount-over-stale-image, and
> the break-glass two-source fetch. The single operator control is the
> kill-switch `[runner.self_heal] enabled = false`, which leaves every seam
> nil and restores the pre-0.3.0 daemon byte for byte. The manifest is
> built from the pinned runner table and signed in CI (`mage manifest:*`,
> `.github/workflows/runner-manifest.yml`); the minisign public key is
> compiled in. See [Operating it](#operating-it) for the two one-time
> manual steps. The auto-PR step is still designed only.
>
> **Correction (2026-11-10, same day).** The first draft of this document
> assumed the runner the fleet runs is the embedded tarball. For Linux —
> the dominant path — it is not: see
> [The Linux official-image path](#the-linux-official-image-path-the-dominant-one).
> Mechanisms 1–3 still apply; what changed is *which* runner tree they
> must verify and *how* a fresh one is put in front of a job.

## The outage this prevents

ephemerd launches one ephemeral `--jitconfig` GitHub Actions runner per job.
A JIT runner cannot self-update: it runs one job and exits. The runner build
it ships is a hardcoded Go const (`RunnerVersion` in
`mage/download/download.go`) with a `pinnedSHA256` map of per-platform
tarball hashes; `mage download` fetches, SHA-verifies and `go:embed`s the
tarballs, and at runtime `pkg/runner` extracts the highest embedded version.

GitHub enforces an **undocumented minimum runner version**. A runner below
it connects, is told `Runner version vX.Y.Z is deprecated and cannot receive
messages.`, and exits without a job. ephemerd sees "runner registered →
runner exited, exit code 0" — which looks like a normal teardown — and
provisions the next one. When the pinned 2.337.0 fell below the floor every
runner did exactly this and the fleet sat at zero throughput for about a day
while jobs queued.

Two things made it a day instead of a minute:

1. **Nothing distinguished "ran a job and exited" from "never got a job and
   exited".** `exit_code=0` is the teardown status of `run.sh`, not a job
   result; keying anything off it is wrong.
2. **The only remedy was a new build** — of ephemerd for the embedded
   runner, and of the `runner-ci-linux` image for the runner Linux jobs
   actually run (see the next section). On 2026-11-10 the image shipped
   2.336.0 and every Linux job died regardless of the embedded pin.

## The Linux official-image path (the dominant one)

`images/runner-ci-linux/Dockerfile` is `FROM
ghcr.io/actions/actions-runner:latest`, so the image carries its own
runner at `/home/runner` (owned by the image's unprivileged `runner` user,
uid/gid 1001, which is also the `USER` the container runs as).
`pkg/runtime.isOfficialRunnerImage` lists `ephpm/ephemerd:runner-ci-linux`
as official, and for an official image `runtime.Create`:

- runs `/home/runner/run.sh --jitconfig …` straight from the image;
- sets `needsRunnerMount = false` — the embedded/extracted runner is
  **never mounted**. It is mounted only for custom images (at
  `/actions-runner`) and on Windows/macOS.

So on every Linux node the runner that talks to GitHub is whatever the
image was built with, and the embedded tarball is irrelevant to it. A
selector over "embedded ∪ cache" alone does not touch the path that
stalled the fleet.

### Why not patch the image

It was tried on metal the same day: editing the runner inside the image's
overlayfs lower layer in place is **unreliable**. The kernel page cache
kept serving the stale files to new container mounts; one job latched the
patched runner after a `drop_caches`, then subsequent jobs reverted.
Layer edits are also invisible to containerd's content store (the digest
no longer matches the bytes), which is exactly the kind of thing the SHA
pinning exists to prevent. Not a mechanism we build on.

### The mechanism: a verified tree mounted over `/home/runner`

ephemerd already has the right machinery for custom images:
`prepareJobRunnerTree` mounts a shared runner tree into the container as
an **overlayfs** with the shared tree as the read-only lowerdir and a
per-job upperdir/workdir (the F1 containment fix — no job can mutate the
shared tree). The self-heal uses that machinery, pointed at the official
runner root:

```
official image, override engaged:
   mount /home/runner  ←  overlay(lowerdir = <verified fresh runner tree>,
                                   upperdir = <data>/runners/job-<id>/upper,
                                   workdir  = <data>/runners/job-<id>/work)
   entrypoint unchanged: /home/runner/run.sh --jitconfig …
```

The mount is set up by runc as part of the container's OCI spec, so it is
new on every job, bypasses whatever the page cache holds for the image
layer, and goes away with the job. The image is never modified. The
`_work/` tree, `.runner`, and the regenerated `run-helper.sh` land in the
per-job upper exactly as they do for custom images, so dind's bind-source
resolution (`bindMappings[/home/runner] = upperdir`) keeps working.

Two details that matter:

- **Ownership.** Two things must carry the image's `runner` uid/gid
  (1001, as the probe reports for `/home/runner`), or the merged
  `/home/runner` is root-owned and `run.sh`'s first write (copying
  `run-helper.sh`, then `.runner`, then `_work/`) fails with EACCES:
  the **lowerdir** (ephemerd extracts runner trees as root;
  `PrepareOverrideTree` chowns the override tree before it is engaged) and
  the **per-job upperdir root**, because overlayfs takes the merged root's
  attributes from the upper root when it exists — and it always exists
  here, `prepareJobRunnerTree` creates it. `matchTreeOwner`
  (`pkg/runtime/treeowner_linux.go`) chowns the fresh upper to the
  lowerdir's owner on every job; for the custom-image path (root-owned
  tree) it is a no-op. The containerd e2e asserts the write as uid 1001.
- **Which tree.** The lowerdir is the best verified tree on the node —
  the embedded extraction when it is new enough (image 2.336.0, embedded
  2.337.0 ≥ floor → no download needed at all), otherwise the verified
  cache entry the break-glass fetch produced. In the 2026-11-10 shape both
  were below the floor, so the fetch was the only way out; the override
  engages the moment the cache has a tree ≥ floor.

### Seam in `pkg/runtime`

`runtime.Config.RunnerOverride` is an interface
(`OfficialRunnerOverride(image) (dir, ok)`); `planRunnerMount` is the
pure decision that replaced the inline `needsRunnerMount` expression. With
a nil source the plan is byte-for-byte the old rule (custom || windows).
With an engaged override and an official image on Linux it returns
`{needsMount: true, dir: <override tree>, mount: /home/runner}`. Windows
is never affected (no official image path there), and a custom image is
never masked (it already gets the configured runner at `/actions-runner`).

### Probing the image

Boot-verify and the zero-work handler need to know what the image ships.
`runtime.ProbeImageRunner(ctx, image)` (Linux only) mounts a read-only
**view** snapshot of the image rootfs with `mount.WithTempMount` — the
same technique `patchSnapshotRunner` uses — and reads
`/home/runner/bin/Runner.Listener.deps.json`, whose `.NET` target entry is
`"Runner.Listener/2.337.0"` (verified against the 2.337.0 archive; it is
the only version marker present in an unconfigured tree, since `.runner`
only exists after `config.sh`). The probe also returns the uid/gid of
`/home/runner` for the ownership fix above. The view is removed
afterwards; nothing is written. The view is created under a short-lived
containerd **lease** so a GC pass cannot remove it (or the layers under
it) mid-probe, and `SweepRunnerProbeViews` removes `ephemerd-runner-probe-*`
views a crashed previous daemon left behind, once at boot before the first
probe.

`ephemerd_runner_image_version_info{image,version}` publishes the result.

### Decision

`DecideImageOverride(image, available, manifest, breakerOpen)`:

| image vs floor | best verified tree on node | result |
|---|---|---|
| ≥ floor | anything | no override (a healthy image is never masked) |
| < floor | newer than image AND ≥ floor | **override** with that tree |
| < floor | nothing newer-and-≥-floor | `NeedFetch` → break-glass fetch, then re-decide |
| no manifest, breaker open | newer than image | override (embedded over image) |
| no manifest, breaker open | nothing newer | `NeedFetch` |
| no manifest, breaker closed | — | nothing; never on a hunch |

The decision is pure; the controller adds the lifecycle around it:

- **A verified manifest is authoritative.** When one says the image is
  at/above the floor, an engaged override is **cleared** — including one
  engaged on the breaker alone during a manifest outage. Without that the
  "no manifest + breaker open" branch was sticky and would have kept the
  `IMAGE RUNNER IS STALE` alert up for a webhook outage.
- **No manifest never clears.** An override engaged while the manifest
  was unreachable stays engaged until a verified manifest arrives; clearing
  it on "breaker closed" would let the next three runners die every poll
  interval to re-learn the same thing.
- **Breaker open + manifest says healthy** is *not* an override: the
  manifest knows the floor better than three refused runners do. The log
  says to republish the manifest with a higher floor if GitHub moved it.
- **Engage resets the linux breaker**, so the next runner (now on a current
  tree) is admitted immediately instead of after the cooldown.
- `NeedFetch` triggers the break-glass fetch when it is available, then the
  decision is re-run on the fresh selection without a second probe.

### Which hosts

- **Linux hosts (coyotes, ducks, sharks)**: host daemon == runtime; the
  host daemon runs the whole subsystem (detector included) and the
  override tree lives in the host data dir and is mounted directly.
- **Windows and macOS hosts**: Linux jobs are dispatched into the sidecar
  VM, whose `--containerd-only` worker daemon runs **its own** self-heal:
  boot-verify, image probe + override (from its embedded runner), and the
  break-glass fetch when its config carries a GitHub **token** (Windows
  stages the host `config.toml` into the VM; an App credential needs a key
  file the VM does not have, so only the PAT form is used). The zero-work
  detector lives on the host (it needs the webhooks), so the
  "no manifest + breaker open" path does not cross into the VM: the VM
  acts on the manifest floor only. The host daemon itself has no
  official-image path there (`Image = ""`), but its detector/breaker still
  gates `linux` dispatch, and its own mounted-path runner (win-x64) is
  verified and fetched like any other.
- The verified-cache runner also takes effect on the **mounted** paths
  (custom Linux images, Windows) on the next job without a restart:
  `Selector.MountedRunnerDir` is consulted per Create through the
  `runtime.MountedRunnerSource` seam, with the per-job writable tree staying
  under the configured runners directory.

### What the alert must say

The override is a bridge. While `ephemerd_runner_image_override_active`
is 1 the node logs, at error level, the fixed line
`runner image override engaged: IMAGE RUNNER IS STALE` naming the image,
the mounted version and the action (rebuild and push `runner-ci-linux`
with a current `actions/runner`, then bump the pin). It is routed as
non-silenceable until the gauge returns to 0, which happens when a probe
of the (rebuilt, re-pulled) image reports a version ≥ floor and the
override clears itself.

The SHA pinning is deliberate supply-chain hardening — fork PRs run as root
inside these runners — so the fix must never run a binary whose hash was
not verified against something ephemerd already trusts. Every path below
preserves that; see [How every path preserves SHA pinning](#how-every-path-preserves-sha-pinning).

## Overview: three mechanisms

| # | Mechanism | Trigger | Effect | Switch (all default ON) |
|---|-----------|---------|--------|--------------------------|
| 1 | **Boot-time verify** | daemon start, then every manifest poll | re-hash the embedded/cached runner, **probe the official Linux image's `/home/runner`**, compare both against the manifest floor; fetch the current one (verified) when below it | `self_heal.enabled` |
| 2 | **Zero-work detector + circuit breaker** | runner exit | count registered→exited-without-a-job; after K in a row on a platform, open the breaker (stop provisioning, queue the jobs for retry), alert | `self_heal.enabled` |
| 3a | **Image override** (Linux) | image probe < floor, or breaker open with a newer tree on the node | mount a verified newer runner tree **over `/home/runner`** on every job until a rebuilt image rolls out; alert | `self_heal.image_override` (requires 1) |
| 3b | **Break-glass verified auto-upgrade** | 3a needs a tree the node does not have, or embedded < floor on a mounted path | the daemon fetches the current runner, verifies it against **two independent sources**, stores it content-addressed; the next job uses it (via 3a on the image path, via the selector on the mounted paths); alerts loudly; (designed: opens a PR to make the pin permanent) | `self_heal.auto_upgrade` (requires 1) |

Mechanisms 3a/3b are bridges: they keep the fleet alive until a rebuilt
`runner-ci-linux` image and a properly pinned ephemerd release roll out.
They do not replace the pin, and the alert says so until they do.

## Signed manifest

A small JSON document published out-of-band (a GitHub release asset on
`ephpm/ephemerd` under a `runner-manifest/<n>` tag, or any HTTPS URL)
alongside a detached signature. ephemerd trusts the manifest only because
the embedded public key verifies the signature.

```json
{
  "schema_version": 1,
  "version": "2.338.2",
  "floor": "2.338.0",
  "channel": "stable",
  "rollback": false,
  "published_at": "2026-11-10T18:00:00Z",
  "platforms": {
    "linux-x64":   { "url": "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz",   "sha256": "baebf204…" },
    "linux-arm64": { "url": "…/actions-runner-linux-arm64-2.338.2.tar.gz", "sha256": "670adb2f…" },
    "win-x64":     { "url": "…/actions-runner-win-x64-2.338.2.zip",        "sha256": "a1f17aa9…" },
    "osx-x64":     { "url": "…/actions-runner-osx-x64-2.338.2.tar.gz",     "sha256": "109fff50…" },
    "osx-arm64":   { "url": "…/actions-runner-osx-arm64-2.338.2.tar.gz",   "sha256": "25a065af…" }
  }
}
```

Field semantics (`pkg/runnerversion/manifest.go`):

- `schema_version` — must be `1`. Unknown versions are rejected, not
  "best-effort parsed".
- `version` — the runner the fleet should be on. Every `platforms` entry
  must be for this version (the filename in `url` must embed it).
- `floor` — the lowest version that still receives jobs, as far as the
  manifest author knows (GitHub does not publish it; we learn it when a
  runner is refused and encode it here). `floor <= version` is enforced. A
  runner `< floor` must not take work.
- `channel` — `stable` or `canary`. A node is configured with a channel
  and ignores manifests for the other one. Lets one node try a new runner
  before the fleet.
- `rollback` — `true` marks this manifest as a deliberate downgrade. The
  selector normally refuses to pick a lower version than the one it is
  on (so a replayed old manifest cannot regress the fleet); `rollback`
  overrides that **only** for a manifest whose `published_at` is newer
  than the one currently applied. Replay protection is `published_at`
  monotonicity, persisted next to the cache.
- `published_at` — RFC 3339. Manifests older than the last applied one
  are ignored (replay protection).
- `platforms` — keyed by the runner's own platform naming
  (`linux|win|osx` × `x64|arm64`), the same scheme `mage/download`
  uses for filenames. `sha256` is lower-case hex of the archive. The
  `url` must be `https`.

The manifest is a **floor and a target**, not a build input: the embedded
runner is still what ships in the binary. The manifest is what lets a
running daemon know the ground has moved.

### Signing

**Day one: minisign.** The manifest is signed with `minisign -S` and the
public key is compiled into ephemerd
(`pkg/runnerversion/keys/runner-manifest.pub`, `go:embed`). Verification
is pure Go: Ed25519 from the standard library plus BLAKE2b-512 from
`golang.org/x/crypto` (already a dependency) for minisign's pre-hashed
(`ED`) mode. The legacy un-hashed (`Ed`) mode is accepted too. Both the
per-file signature and minisign's global signature over
`signature || trusted comment` are checked, and the key ID in the
signature must match the embedded key.

Why minisign and not GPG: a 42-byte public key, a two-line signature file,
no keyring, no web of trust, nothing to configure on the node. Why not
`ssh-keygen -Y sign`: equally good (Ed25519, `golang.org/x/crypto/ssh`
can verify the `SSHSIG` envelope), and a reasonable alternative if the
operator would rather use an SSH key they already have in a hardware
token. The parser is ~100 lines either way; the embedded-key model is the
same.

**Heavier alternative: Sigstore keyless.** Sign with `cosign sign-blob`
using the GitHub Actions OIDC identity of the release workflow; ephemerd
verifies the certificate chain against the Fulcio root and checks the
Rekor inclusion proof. Benefits: no private key to lose. Costs: a large
dependency tree (`sigstore-go`), network access to Rekor at verify time
(or a bundled inclusion proof), and a trust root that itself needs
updating. Not worth it until there is more than one signer. The manifest
format does not change if we move to it later; only `Verifier` does.

**The trust root.** `pkg/runnerversion/keys/runner-manifest.pub` is a real
key since 0.3.0 (key id `CCFDB74C12C7F433`, pinned by
`TestEmbeddedPublicKeyIsReal` so a rotation is a reviewed change to both).
It was generated with `mage manifest:keygen`, which uses the same code
that verifies (`GenerateKeypair`) and writes minisign-compatible `.pub` and
unencrypted `.key` files (`minisign -G -W` layout; a password-protected
key is rejected with a clear error — the CI secret is a machine credential
with nobody to type a password).

**The pipeline.** `mage manifest:build` composes `dist/runner-manifest.json`
from `mage/download`'s pinned table (`RunnerVersion`, `RunnerFloor`,
`pinnedSHA256` via `RunnerArtifacts()` — the hashes are never re-typed) and
round-trips it through the daemon's own parser. `mage manifest:sign` signs
it in pure Go (`SecretKey.Sign`, pre-hashed `ED` mode, minisign's
`timestamp:… file:… hashed` trusted comment) with the key from
`RUNNER_MANIFEST_MINISIGN_KEY` (the GitHub Actions secret; contents of the
`.key` file or its base64 line) or `RUNNER_MANIFEST_MINISIGN_KEY_FILE`.
`mage manifest:verify` then checks the pair against the **compiled-in** key,
so a secret that does not match the trust root fails in CI, not on every
node. `.github/workflows/runner-manifest.yml` runs the three on every
published ephemerd release and on `workflow_dispatch` (inputs: `floor`
override, `channel`, `rollback`) and uploads both files to a rolling
prerelease whose tag never changes (`runner-manifest`, or
`runner-manifest-canary`), marked not-latest so it cannot take the
`releases/latest` slot `ephemerd upgrade` follows. That gives the stable
URL every node carries by default
(`config.DefaultSelfHealManifestURL`). The signing key never lives on a
node.

**Rotation.** `mage manifest:keygen` into a scratch directory, commit the
new `.pub` over `keys/runner-manifest.pub` and update the pinned key id in
the test, replace the repository secret, republish the manifest, then roll
the release: nodes on the old binary keep verifying the old manifest until
they upgrade (the old `.minisig` stays valid for them only if you keep
publishing one; usually you do not, and they run on what verified).

`[runner.self_heal] public_key` can override the embedded key (a single
base64 line, as in the `.pub` file). It exists for key rotation and
testing, not as the normal path — a node whose config can be edited can
already be made to run anything.

## Boot-time verify flow

Runs from `runnerversion.Controller.Run`, started by `cmd/ephemerd serve`
in the background right after the runtime and providers exist, and again on
every manifest poll (default hourly) and on every breaker trip. It does not
delay the scheduler: until the first pass completes the node runs what it
has today, which is also what it would run with the subsystem off. Both the
host daemon and the in-VM worker (`--containerd-only`) run it; the worker
has no detector (no scheduler) but owns the Linux image path on
Windows/macOS hosts.

```
                 ┌───────────────────────┐
                 │ load manifest + .minisig │ ── fail ──▶ log, metrics, serve on
                 │ verify sig (embedded key)│            embedded (same as today)
                 └──────────┬──────────────┘
                            │ ok
          ┌─────────────────▼───────────────────┐
          │ candidates = embedded ∪ verified cache │
          │ for each: re-hash archive, compare to  │
          │ manifest.platforms[me].sha256 OR the   │
          │ sidecar sha recorded at fetch time     │
          └─────────────────┬───────────────────┘
                            │
          ┌─────────────────▼───────────────────┐
          │ best = highest version whose hash     │
          │ verified; embedded is the last resort │
          └─────────────────┬───────────────────┘
                            │
                best.version >= manifest.floor ?
                 │ yes                    │ no
                 ▼                        ▼
        serve jobs on best      self_heal.auto_upgrade ?
                                 │ yes             │ no
                                 ▼                 ▼
                      fetch+verify+swap     cordon (refuse work),
                      (see below), then     alert "runner below floor",
                      serve on it           ephemerd_runner_version_info
                                            {source="embedded-below-floor"}
```

Re-hashing the embedded archive at boot costs ~0.3 s for a 150 MB tarball
and is what detects a tampered or truncated on-disk extraction source. The
embedded archive's hash is compared against the manifest when the manifest
lists that version, and otherwise only against itself (the embed is
trusted by construction: it was pinned at build time).

A manifest fetch failure is **not** fatal and does not change behaviour:
the daemon runs on whatever verified today, exactly as if the subsystem
did not exist. A manifest that fails signature verification is logged at
error level and counted, and is also ignored. Only a verified manifest can
change what runs.

"Below floor with the fetch unavailable" (auto-upgrade off, or no GitHub
credential on this daemon) logs at error level with the action and leaves
the stop-the-thrash job to the zero-work breaker: the first three refused
runners open it and provisioning pauses on their own evidence. An explicit
cordon was considered and deferred — it would suspend every platform on
the host, including ones whose runner is fine (a Windows host's Linux VM
jobs are on the image path, not the host's win-x64 runner).

## Zero-work detector

### Definition

A **zero-work runner** is one for which all of the following hold:

1. ephemerd registered it (JIT config minted, container/VM started) at
   time `t0`;
2. the runner process exited on its own at `t1` — not killed by ephemerd
   (job timeout, drain, orphan sweep, shutdown all count as "we killed
   it" and are excluded);
3. `t1 − t0 < window` (default **120 s**; a refused runner exits within
   a few seconds of connecting, while the shortest real job on this fleet
   including runner start is well over a minute);
4. **no job-started signal** was observed for that runner name: no
   `workflow_job.in_progress` webhook naming it (that is the
   `runnerBinding.bound` flag the scheduler already maintains for
   fungibility), and no provider poll reporting it busy.

`exit_code` is **not** an input. Runner `run.sh` exits 0 after a refused
connection, after a completed job, and after a cancelled job alike.

The detector needs the job-started signal to be *observable*. For
providers that do not report runner names (`providers.RunnerNameReporter`
false) their runners are never registered with the detector, the same
restriction the orphan sweep already has — otherwise every short job would
count as zero-work. Two signals count as job-started: the `in_progress`
event naming the runner (webhook, or the poll's per-cycle report — the
scheduler binds on the first), and a **busy probe** that says Busy from any
layer (`probeRunnerBusy`: local container/VM introspection, then the
provider's busy flag). The second is what keeps a webhook outage from
tripping the breaker on runners that are in fact working: the orphan sweep
consults it for every unbound runner.

Wiring (`pkg/scheduler`, `Config.RunnerHealth`): `Registered` from
`trackRunning` with the dispatch class (`linux` for VM dispatch, `macos`
for the per-job VM, else the host OS); `JobStarted` from `handleInProgress`
and the busy probe; `Exited` from the three wait-goroutines with
`killedByUs = jobCtx.Err() != nil` (timeout, drain, completed-event
teardown, orphan sweep and shutdown all cancel the job context);
`Allow(platform)` as the last gate in `admitDispatch`, after the cordon
and satisfied/gone checks, so a job the breaker refuses is queued on the
**retry ladder** (`errBreakerOpen`) and comes back on its own once the
breaker closes rather than waiting for the 30-minute reconcile sweep.

### Corroboration

Optionally, when a runner is judged zero-work and its working directory
is still on disk (local Linux/Windows path; not the dispatched-VM path
where the directory lives in the guest), scan `_diag/Runner_*.log` for
the refusal line (`deprecated and cannot receive messages`,
case-insensitive). A hit upgrades the verdict's `reason` from
`zero-work` to `deprecated` and is logged verbatim. The breaker does not
require corroboration — a floor bump could change the message — but a
corroborated verdict lets the alert say exactly what happened.

### Circuit breaker

Per platform (`linux`, `windows`, `macos`, i.e. the dispatch class), the
detector keeps a **consecutive** zero-work streak. Any runner on that
platform that receives a job-started signal resets the streak to zero and
closes the breaker.

```
CLOSED ──(streak >= threshold)──▶ OPEN ──(cooldown elapsed)──▶ HALF-OPEN
  ▲                                                               │
  │◀──────────── job-started on this platform ◀──────────────────┤ (one probe allowed)
  │                                                               │
  └──────────────────────────────────────────── probe zero-work ──┘ → OPEN again
```

- `threshold` K default **3**. One refused runner is noise (a flaky
  network can produce it); three in a row with no job in between is not.
- While OPEN, `Allow(platform)` is false: the scheduler should not
  provision for that platform. Queued jobs stay queued on GitHub (they
  are not lost; GitHub keeps them up to 24 h) and the retry queue holds
  them locally.
- `cooldown` default **15 m**: then HALF-OPEN lets exactly one runner
  through as a probe. If it gets a job the breaker closes; if it is
  zero-work the breaker reopens.
- On the CLOSED→OPEN transition the detector calls `OnDegraded(platform,
  streak, reason)` once, which the controller installs as a **kick**: the
  verify/decide/act pass runs now (re-probe, fetch if needed, engage) rather
  than at the next hourly poll. Engaging an override or swapping in a
  fetched runner resets that platform's breaker so the next runner is
  admitted immediately.

Metrics: `ephemerd_runner_zero_work_total{platform}` increments on every
verdict; `ephemerd_runner_breaker_open{platform}` is 1 while OPEN or
HALF-OPEN.

### What the detector is not

It cannot tell *why* a runner got no job. A GitHub incident, a label
mismatch, or a webhook outage produce the same shape. That is fine: the
breaker's job is to stop the thrash and page a human; the auto-upgrade's
job is to try the one remedy that is safe to try without one. If the
remedy does not help (the runner is current and still refused), the
breaker reopens on the probe and the alert stays up.

## Verified fetch, content-addressed cache, atomic swap, select

Everything in `pkg/runnerversion/store.go` and `fetch.go`. Layout under
the daemon's data directory:

```
<data>/runners/verified/
  blobs/<sha256>.tar.gz          the archive, named by its hash
  blobs/<sha256>.json            sidecar: version, platform, url, sha, fetched_at, sources[]
  extracted/<version>-<goos>/    extracted tree, ".extracted" marker last
  current                        pointer file: {"sha","version","ext","dir"}
  previous                       the pointer `current` replaced
  applied-manifest.json          published_at of the last applied manifest (replay guard)
```

### State machine

```
 Resolve ──▶ Agree ──▶ Fetch ──▶ Verify ──▶ Commit ──▶ Extract ──▶ Swap ──▶ Prune
    │          │         │          │          │           │          │
    ▼          ▼         ▼          ▼          ▼           ▼          ▼
  need       two       stream    hash ==     rename     tar/zip to  rename     delete every
  version    sources   to tmp    expected?   tmp →      extracted/  pointer    blob and dir
  for my     must      in blobs/ else        blobs/     (path-      current →  not referenced
  platform   agree on  hashing   delete tmp  <sha>.ext  traversal   previous,  by current or
             sha+ver   as we go  and FAIL    + sidecar  checked)    new →      previous
                                                                    current
```

Invariants, in order of importance:

1. **Verify before extract, always.** Nothing in `blobs/` is extracted
   until its hash was checked over the complete file; an archive that
   fails is deleted and the failure counted. `extracted/` is derived data
   and can be rebuilt from `blobs/`.
2. **Bounded cache: current + previous.** `Prune` runs after every swap
   and at boot, and removes every blob and extracted directory that
   neither pointer references. The fleet has died from full disks
   before; this cache can hold at most two runners (~300 MB) plus one
   in-flight download. Before a fetch, `diskspace.Check` on the cache
   path must report at least `min_free_gb` (default 5) free or the fetch
   is refused with a specific error.
3. **Size cap on the wire.** The download is read through a limited
   reader (`max_archive_mb`, default 512) and the response
   `Content-Length`, when present, is checked against it before reading.
   The runner is ~150–200 MB; a 10 GB response is an attack or a bug.
4. **Single-flight per hash.** Concurrent requests for the same SHA share
   one download; different SHAs are serialized by a store-wide lock so
   two upgrades cannot interleave their swaps.
5. **Atomic swap.** The pointer file is written to a temp name and
   renamed over `current` (Go's `os.Rename` replaces on Windows too).
   A reader sees the old pointer or the new one, never a torn one. The
   previous pointer is kept so a rollback is a pointer rename, no fetch.
6. **Never swap to a lower version** unless the manifest has
   `rollback: true` and is newer than the last applied one.
7. **Crash safety.** Temp files carry a `.tmp-` prefix and are deleted at
   boot. A crash after Commit but before Swap leaves an unreferenced blob,
   which Prune removes. A crash mid-extract leaves a directory without
   its `.extracted` marker, which the selector ignores and Prune removes.

### Selection: embedded ∪ verified cache

`pkg/runnerversion.Selector` wraps `*runner.Manager` (the embedded
extractor) and the store:

- candidates = `{embedded version, from runner.Version}` ∪ `{store.current}`
  (only if its pointer resolves to an extracted dir with a marker);
- pick the **highest** by `runner.CompareVersions` — the same comparison
  `findTarball` already uses to prefer the newest embedded archive;
- the embedded runner is the **last-resort floor**: if the store is
  empty, corrupt, or lower, the selector returns the embedded dir and
  the daemon behaves exactly as before this subsystem existed;
- the result carries `Source` (`embedded` or `verified-cache`) which is
  the `source` label on `ephemerd_runner_version_info`.

The selector does not *replace* `runner.Manager`; it sits in front of
it. It governs the **mounted** paths only — custom images, Windows,
macOS — and is the source of the override tree for the official-image
path. The integration points, all wired in `cmd/ephemerd/selfheal.go`:

1. `runtime.Config.RunnerDir` stays the embedded tree (the floor, and the
   parent of every per-job tree); `runtime.Config.RunnerOverride` is one
   value (`runnerSource`) that implements both seams — the
   `*ImageOverride` for official images and the selector's
   `MountedRunnerDir()` (`runtime.MountedRunnerSource`) for the mounted
   paths, consulted on every Create;
2. `Selector.ProbeImage` is `rt.ProbeImageRunner` bound to the daemon's
   Linux job image (`[runner] default_image`, else the runtime default;
   `""` when that is not an official image or this daemon runs no Linux
   containers);
3. `Selector.BreakerOpen` reads the detector's `linux` breaker.

## Break-glass auto-upgrade

`[runner.self_heal] auto_upgrade` (default **true**; meaningful only with
`enabled`; runs in the ephemerd daemon — host or in-VM worker — never in a
runner container). When it is off, or when the daemon has no GitHub
credential for the cross-check, the controller logs that the fetch is
unavailable and the override can still use the embedded tree.

### Trigger

Either the breaker's `OnDegraded` for any platform, or a routine manifest
poll finding `selected.version < manifest.floor`. Both go through the
same `Upgrader.Run(ctx, platform)`.

### Two-source verification

The upgrade must not depend on any single party being honest:

| Source | What it gives | Trust |
|--------|---------------|-------|
| **Signed manifest** | `version`, per-platform `url` + `sha256` | operator-signed, verified with the embedded key |
| **GitHub `GET /repos/{owner}/{repo}/actions/runners/downloads`** (or `/orgs/{org}/…`) | per-platform `download_url`, `filename`, `sha256_checksum` | GitHub's own statement of the current runner, over authenticated TLS with the node's existing token |

Rules, in `pkg/runnerversion/upgrader.go`:

1. Resolve the platform's entry from both. The version parsed from
   GitHub's `filename` must equal `manifest.version`, and GitHub's
   `sha256_checksum` must equal `manifest.platforms[p].sha256`. Any
   disagreement is a hard stop with an alert: either the manifest is
   stale (GitHub moved on) or someone is lying. A stale manifest is the
   common case; the alert says "re-sign the manifest for X.Y.Z".
2. Fetch from the manifest's `url` (not GitHub's `download_url` — they
   should be the same release asset, but the signed one wins).
3. Stream-hash; the result must equal the agreed SHA. Only then commit,
   extract, swap.
4. Optionally (third opinion, best-effort, never required) compare
   against the release asset's `digest` from
   `GET /repos/actions/runner/releases/tags/v{version}`.

Only a *verified* manifest can even be consulted. A node that cannot
verify a manifest cannot auto-upgrade, by construction.

### After a swap

- Emit `ephemerd_runner_version_info{version="2.338.2",source="verified-cache"} 1`
  and log at **error** level with a fixed, grep-able message
  (`runner auto-upgraded: PIN IS STALE`). This alert is routed as
  non-silenceable: it stays up until `ephemerd_runner_version_info`
  reports `source="embedded"` again, i.e. until a release with the new
  pin is deployed.
- **Auto-open a PR** (designed, not yet implemented): the host daemon
  uses its GitHub credential (the same App installation that mints JIT
  configs; it needs `contents:write` + `pull_requests:write` on
  `ephpm/ephemerd`, which is an installation-permission change to make
  consciously) to push a branch `auto/runner-pin-<version>` that updates
  `RunnerVersion` and the five `pinnedSHA256` entries with the SHAs it
  verified (from GitHub `runners/downloads`, cross-checked against the
  manifest), and opens a PR titled `chore(runner): pin actions/runner
  <version> (auto, fleet was below floor)`. The body carries both
  sources' hashes, the node that opened it, and a **deadline**
  (`auto_upgrade_pr_deadline`, default 72 h). Past the deadline with the
  PR unmerged, every node still on `source="verified-cache"` escalates
  the alert (second fixed message). The PR is opened once: nodes check
  for an open PR with that title before creating one. The
  `ci/runner-bump-workflow` branch already prototypes the file edit; the
  auto-PR reuses that rather than a second editor.
- The verified-cache runner keeps serving across daemon restarts (the
  pointer file persists) until an ephemerd whose **embedded** version is
  `>=` the cached one starts, at which point the selector prefers
  embedded only when it is strictly higher — equal versions prefer
  embedded, so the deploy of the pinned release naturally retires the
  cache entry and Prune removes it.

## GitHub primitives (verified 2026-11-10)

| Primitive | What it is | Confidence |
|-----------|------------|------------|
| `GET /repos/{owner}/{repo}/actions/runners/downloads` and `GET /orgs/{org}/actions/runners/downloads` | Array of `{os, architecture, download_url, filename, temp_download_token?, sha256_checksum?}`. `sha256_checksum` is documented as optional (string) but is present for every runner asset in practice. Needs `repo` (repo endpoint) / `admin:org` (org endpoint) or the App `administration:read` permission ephemerd already has for JIT registration. go-github v72 models it as `RunnerApplicationDownload.SHA256Checksum`. | High — docs read today; field present in current responses |
| Release asset `digest` | `GET /repos/actions/runner/releases/tags/v2.338.2` returns `assets[].digest` = `sha256:<hex>` for every runner archive. The docs list the field as `string or null` with no description; it was added in 2025 and is populated for new uploads. | Medium-high — observed populated on v2.338.2 today; undocumented semantics, treat as a third opinion only |
| Release-notes SHA section | Each runner release body lists `- actions-runner-<os>-<arch>-<ver>.<ext> <!-- BEGIN SHA <os>-<arch> -->hex<!-- END SHA … -->`. This is what `pinnedSHA256`'s comment cites as its source. | High for format today; it is prose, could change without notice, so never a machine source of truth |
| Minimum runner version | **Not published.** GitHub refuses old runners with the message above. The manifest `floor` encodes what we learn empirically. | n/a |

`sha256_checksum` on `runners/downloads` and `digest` on the release asset
are both produced by GitHub from the same upload, so they are not fully
independent of each other; the independence that matters is manifest
(operator) vs GitHub.

## Metrics

Declared in `pkg/metrics/runnerversion.go`:

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `ephemerd_runner_version_info` | gauge (=1) | `version`, `source` (`embedded`, `verified-cache`, `embedded-below-floor`) | which runner this node serves and where it came from. Exactly one series is 1 at a time; `SetRunnerVersion` clears the previous one |
| `ephemerd_runner_pin_age_seconds` | gauge | – | now − `published_at` of the **embedded** pin's `actions/runner` release. Alert at, say, 45 d: GitHub's floor has historically trailed the current release by a few minor versions |
| `ephemerd_runner_zero_work_total` | counter | `platform` | zero-work verdicts |
| `ephemerd_runner_breaker_open` | gauge | `platform` | 1 while the breaker is OPEN or HALF-OPEN |
| `ephemerd_runner_image_version_info` | gauge (=1) | `image`, `version` | what the official Linux image's `/home/runner` ships, as probed |
| `ephemerd_runner_image_override_active` | gauge | – | 1 while a verified tree is mounted over `/home/runner`; alert while 1 (image rebuild outstanding) |

**Known prerequisite.** mayfly never renders `[metrics] bind_addr` into
the node config, so every node's metrics listener binds `127.0.0.1` and
Prometheus scrapes nothing (see the 2026-09 fleet cache audit). Until
that is fixed, these metrics are visible only via `ephemerd metrics` on
the node, and the alerts above must also go through the log-based path.
Fixing the render is a mayfly change outside this subsystem; it is listed
here so nobody designs an alert on a series that is never scraped.

## Configuration

Everything has a built-in default; a node with no `[runner.self_heal]`
table (or an empty one) runs the full subsystem. The values below are the
defaults.

```toml
[runner.self_heal]
enabled = true                 # THE KILL-SWITCH. false turns everything below off without a redeploy
manifest_url = "https://github.com/ephpm/ephemerd/releases/download/runner-manifest/runner-manifest.json"
                               # ".minisig" is appended for the signature
public_key = ""                # override the compiled-in minisign public key (base64 line); rotation/testing only
channel = "stable"             # "stable" | "canary"; manifests for the other channel are ignored
poll_interval = "1h"           # manifest re-check cadence
zero_work_window = "120s"      # registered→self-exit under this with no job-started = zero-work
zero_work_threshold = 3        # consecutive zero-work runners per platform that open the breaker
breaker_cooldown = "15m"       # OPEN → HALF-OPEN
image_override = true          # Linux: mount a verified newer runner over a stale official image's /home/runner
auto_upgrade = true            # break-glass two-source verified fetch
max_archive_mb = 512           # refuse larger downloads
min_free_gb = 5                # refuse to fetch below this much free space on the cache volume
auto_upgrade_pr_repo = ""      # "ephpm/ephemerd"; empty disables the auto-PR (not implemented yet)
auto_upgrade_pr_deadline = "72h"
```

**Why `*bool`.** TOML decodes a missing key as the zero value, and a plain
`bool`'s zero value is `false`, which would make the product default off.
`Enabled`, `AutoUpgrade` and `ImageOverride` are therefore `*bool` with
`ResolvedEnabled()` / `ResolvedAutoUpgrade()` / `ResolvedImageOverride()`
(nil → true, and the two sub-switches are false whenever the kill-switch
is). This is the same convention `[vm.linux] enabled` and
`[dind] allow_privileged` already use.

**Validation** (`SelfHealToml.Validate`, called from `config.Load` so a bad
table fails the daemon at start, not at the first poll): `auto_upgrade = true`
or `image_override = true` with `enabled = false` is an error (the
sub-switches are meaningful only when enabled); `manifest_url` must be
`https://`; durations must parse and be positive; `channel` must be one of
the two values; threshold/size fields non-negative. With `enabled = false`
only the two "requires enabled" checks run, so a stray tuning key cannot
keep an opted-out node from starting. An unparsable `public_key` fails at
daemon start as well.

## Failure modes

| Failure | Behaviour |
|---------|-----------|
| Manifest URL unreachable | log warn, `ephemerd_runner_manifest_errors_total{reason="fetch"}`; run on what verified; retry next poll |
| Signature invalid / key mismatch / schema unknown | log **error**, counted `reason="signature"` / `"schema"`; manifest ignored entirely |
| Manifest older than last applied | ignored (replay) |
| GitHub `runners/downloads` disagrees with manifest | no fetch; alert "manifest stale or sources disagree"; breaker stays open |
| Download hash mismatch | archive deleted, counted, alert; no swap; next poll retries (a CDN serving a stale asset will keep failing and keep the alert up — correct) |
| Disk below `min_free_gb` | fetch refused with a specific error; alert; existing runner keeps serving |
| Crash during fetch/extract/swap | see crash safety above; worst case an unreferenced blob until the next Prune |
| Verified-cache runner is itself refused by GitHub | breaker reopens on the probe; the upgrader sees `selected.version == manifest.version` and does nothing (no loop); alert stays up for a human |
| Image probe fails (no `deps.json`, view mount error) | reported in `BootReport.ImageErr`, image treated as unknown: **no override on a guess**; the node runs the image as today and the alert says the probe failed |
| Override engaged but the image's `runner` uid differs from the tree's owner | the probe's uid/gid is applied to the tree before engaging; if chown fails the override is refused with the error, not engaged broken |
| Override engaged, then the image is rebuilt and re-pulled | next poll's probe reports ≥ floor → override clears, gauge → 0, alert resolves; the per-job overlay simply stops being added |
| Windows-host Linux VM | the override tree is not visible in the VM; the override is not engaged there (documented gap, see "Which hosts") |
| `in_progress` webhooks stop arriving (webhook outage) | a runner the busy probe sees working still counts as job-started (the orphan sweep probes every unbound runner), so working runners do not trip the breaker. Genuinely idle runners still do → breaker opens, alert says "or webhook delivery is broken", the half-open probe re-tests every cooldown, and jobs are queued on the retry ladder, not dropped. Worst case is recoverable by config: `enabled = false` |
| Manifest unreachable AND breaker open (the sticky case) | the embedded tree, if newer than the image, is mounted over it on the breaker's evidence alone; that override is deliberately **kept** while no manifest can be verified and is **cleared the moment a verified manifest says the image is at/above the floor** — the alert cannot outlive a manifest outage |
| Breaker open but a verified manifest says the image is healthy | no override (the manifest is authoritative); the log says to republish the manifest with a higher floor if GitHub raised its minimum (Runner manifest workflow, `floor` input), otherwise to check webhook delivery / GitHub status |
| Manifest never published (secret not set, workflow never run) | every poll logs "manifest unavailable; no verified manifest yet (floor unknown)" at warn and counts `reason="fetch"`; the node runs exactly as with the subsystem off, plus the breaker and the breaker-driven override |
| `auto_upgrade` on, breaker open, no manifest at all | no fetch (nothing verified to fetch); the override still engages the embedded tree when it is newer than the image |
| Daemon has no GitHub credential (in-VM worker without a staged token; non-GitHub providers) | fetch unavailable, logged once at start; the override is limited to the embedded tree |

## How every path preserves SHA pinning

- **Build**: unchanged. `mage download` still refuses to embed an archive
  whose hash is not in `pinnedSHA256`. This subsystem does not touch
  `mage/download/download.go`.
- **Boot**: the embedded archive is re-hashed and must match the manifest
  entry for its version when one exists. A cached archive is re-hashed
  and must match the SHA recorded in its sidecar (which was itself the
  manifest+GitHub-agreed value) — a blob that changed on disk is deleted.
  An archive that does not verify is never extracted and never selected.
- **Manifest**: only a signature-verified manifest is parsed past the
  header, and only a verified manifest can supply a hash to compare
  against. Verification uses a public key compiled into the binary; the
  binary is itself SHA-verified by `ephemerd upgrade`.
- **Fetch**: no bytes are written anywhere but a temp file until the
  hash over the complete stream equals the hash that two independent
  sources agreed on; the temp file is deleted on any mismatch. The
  archive is addressed by that hash from then on, so a later swap cannot
  pick up different bytes under the same name.
- **Extract**: only from a committed blob; path-traversal checks are the
  same ones `pkg/runner` uses.
- **Select**: a candidate is only eligible if its on-disk archive
  verified this boot. The embedded runner, whose pin was verified at
  build time, is the floor.
- **Image override**: the tree mounted over `/home/runner` is one of
  those same verified candidates — never anything else — and the image
  layer is never written to. Masking is by a fresh per-job overlay mount,
  which is also what makes it reliable where a layer edit was not.
- **Rollback**: a pointer swap between two already-verified blobs; no new
  bytes.
- **Auto-PR**: proposes the exact hashes that were verified; a human
  merges; the next release re-verifies at build time. The daemon never
  edits its own pin.

At no point does any path run a runner binary whose SHA-256 was not
checked against a value ephemerd trusted before the bytes arrived.

## Operating it

Two one-time steps make the manifest exist; until then every node boots,
logs "no verified manifest yet" hourly and self-heals on the breaker path
only.

1. **Add the signing secret.** The repository secret
   `RUNNER_MANIFEST_MINISIGN_KEY` on `ephpm/ephemerd` is the contents of
   the `runner-manifest.key` file that pairs with the compiled-in public
   key (`CCFDB74C12C7F433`). It was generated off-repo; it is never
   committed. If it is lost, rotate (see Signing).
2. **Publish the manifest once.** Run the `Runner manifest` workflow
   (`workflow_dispatch`, defaults) — or publish any ephemerd release, which
   triggers it. It creates the rolling `runner-manifest` prerelease with
   `runner-manifest.json` + `.minisig`. Nodes pick it up on their next
   poll (hourly) or restart.

Day-to-day:

- **GitHub refused a runner and the floor is stale:** re-run the workflow
  with `floor` set to the lowest version GitHub still accepts (and bump
  `RunnerFloor` in `mage/download/download.go` in the same change so the
  next release carries it). Nodes override/fetch within a poll.
- **False positive / want it off:** `[runner.self_heal] enabled = false`
  and restart; everything (breaker included) is gone. `image_override =
  false` or `auto_upgrade = false` turn off one mechanism each.
- **Watch:** `runner image override engaged: IMAGE RUNNER IS STALE` and
  `runner auto-upgraded: PIN IS STALE` are the two error-level, grep-able
  lines; `ephemerd_runner_image_override_active`,
  `ephemerd_runner_breaker_open{platform}` and
  `ephemerd_runner_version_info{source}` are the gauges (see the metrics
  prerequisite above about `bind_addr`).
- **Make it permanent:** rebuild and push `runner-ci-linux` with a current
  `actions/runner`, and merge a release that bumps the pin; the override
  clears itself on the next probe of the rebuilt image and the cache entry
  retires when the embedded version catches up.

## What 0.3.0 wires

- `pkg/runnerversion/controller.go` — `Controller`: manifest load (keeps
  the last verified one across failures, ignores other channels),
  `BootVerify` at boot / poll / breaker kick, mounted-path fetch when below
  floor, image decision → `PrepareOverrideTree` + `Engage` / `Clear` /
  fetch-then-engage, breaker reset on engage/swap, the verified-manifest
  auto-clear.
- `pkg/runnerversion/minisign_secret.go` — unencrypted minisign secret
  keys (`GenerateKeypair`, `ParseSecretKey`, `Encode`, `Sign`), so CI signs
  in pure Go and keys are generated by the code that verifies them.
- `pkg/runnerversion/keys/runner-manifest.pub` — the real trust root.
- `mage/manifest` + `mage/download.RunnerArtifacts/RunnerFloor` +
  `.github/workflows/runner-manifest.yml` — build, sign, verify, publish.
- `pkg/config/selfheal.go` — default-on `*bool` switches, built-in
  manifest URL, `Validate` called from `Load`.
- `pkg/runtime` — `MountedRunnerSource` seam (verified cache on the
  mounted paths without restart), lease + boot sweep + namespace fix in
  `ProbeImageRunner`, `matchTreeOwner` for the per-job upperdir.
- `pkg/scheduler` — `Config.RunnerHealth`: `Registered`/`JobStarted`
  (webhook + busy probe)/`Exited`/`Allow` with the breaker as a dispatch
  gate that queues refused jobs for retry.
- `cmd/ephemerd/selfheal.go` — assembly for both daemons; the kill-switch
  leaves `RunnerOverride` and `RunnerHealth` nil.
- `test/e2e/runner_override_test.go` — containerd e2e: a job on
  `ghcr.io/actions/actions-runner:2.336.0` reports the image's runner
  without the override and the host's newer runner with it, can write at
  the top of `/home/runner` as uid 1001, the write lands in the per-job
  upperdir under the runners dir and never in the shared tree, and the
  probe leaks no view. Root-gated like the rest of the e2e suite; CI prints
  its PASS or SKIP verbosely.

**Still not implemented:** the auto-PR (`auto_upgrade_pr_repo` is parsed
and otherwise ignored); `ephemerd_runner_pin_age_seconds` (needs the
embedded pin's release date); surfacing the override/breaker state on
`/healthz` and the Status RPC; host→VM blob push for Windows/macOS hosts
(the VM fetches for itself when it has a token instead).

## What #220 implemented (the engine)

- `pkg/runnerversion/manifest.go` — `Manifest`, `Parse`, `Validate`,
  `PlatformKey`.
- `pkg/runnerversion/minisign.go` — minisign public-key and signature
  parsing, `Verify` (Ed25519 + BLAKE2b-512 prehash, global signature,
  key-ID check); `keys/runner-manifest.pub` + `EmbeddedPublicKey` (a
  placeholder in #220, the real key since 0.3.0).
- `pkg/runnerversion/store.go` — content-addressed store with sidecars,
  `current`/`previous` pointers, atomic `Swap`, `Rollback`, `Prune`,
  boot `Verify` (re-hash), temp cleanup.
- `pkg/runnerversion/fetch.go` — `Fetcher.Fetch`: free-space guard, size
  cap, stream hash, verify-before-commit, single-flight per SHA.
- `pkg/runnerversion/github.go` — `GitHubSource` over go-github
  `runners/downloads`.
- `pkg/runnerversion/upgrader.go` — two-source agreement + fetch +
  extract + swap, gated on `AutoUpgrade`; returns `ErrDisabled` otherwise.
- `pkg/runnerversion/selector.go` — embedded ∪ verified-cache selection
  with embedded as floor; `Dir()/Version()/Source()`.
- `pkg/runnerversion/detector.go` — zero-work detector + per-platform
  breaker + `OnDegraded`.
- `pkg/runnerversion/diag.go` — `_diag` corroboration scan.
- `pkg/runnerversion/imageprobe.go` — `ProbeRunnerTree` /
  `ParseListenerDepsVersion` (version + owner of any runner tree).
- `pkg/runnerversion/override.go` — `ImageOverride` (the engaged state
  the runtime consults), `DecideImageOverride`, `PrepareOverrideTree`.
- `pkg/runtime/runneroverride.go` — `RunnerOverrideSource`,
  `planRunnerMount`; `Create` now takes its mount decision from the plan
  (`runtime.Config.RunnerOverride`, nil = unchanged behaviour).
- `pkg/runtime/runnerprobe_linux.go` — `Runtime.ProbeImageRunner` (view
  snapshot + temp mount + `ProbeRunnerTree`). Needs containerd; covered
  by e2e, not unit tests.
- `pkg/runner/version.go` — **new file**: exported `ParseVersion`,
  `CompareVersions`, `EmbeddedArchive`, `ExtractArchive` wrappers around
  the existing unexported helpers. No existing function in `pkg/runner`
  is modified.
- `pkg/metrics/runnerversion.go` — the four metrics.
- `pkg/config/selfheal.go` — `[runner.self_heal]`, defaults, validation.
  One field added to `RunnerConfig`.

Of the eight follow-ups #220 listed, 0.3.0 does (2)–(6) as described
above; (1) is done through the `MountedRunnerSource` seam instead of
replacing `RunnerDir`; (7) the auto-PR and (8) the host→VM blob push
remain open.
