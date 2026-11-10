# Self-Healing Runner Version

> **Status: foundation implemented, flag-off.** The signed manifest, the
> verified fetch + content-addressed cache + atomic swap, the zero-work
> detector + circuit breaker, and the three metrics live in
> `pkg/runnerversion` (+ `pkg/metrics/runnerversion.go`,
> `pkg/config/selfheal.go`). Nothing is wired into `cmd/ephemerd` yet and
> `[runner.self_heal] enabled` defaults to `false`. The break-glass
> auto-upgrade (`auto_upgrade`) is additionally gated and defaults to
> `false`. The auto-PR step is designed here but not implemented.

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
2. **The only remedy was a new ephemerd release**, because the runner is
   embedded at build time.

The SHA pinning is deliberate supply-chain hardening — fork PRs run as root
inside these runners — so the fix must never run a binary whose hash was
not verified against something ephemerd already trusts. Every path below
preserves that; see [How every path preserves SHA pinning](#how-every-path-preserves-sha-pinning).

## Overview: three mechanisms

| # | Mechanism | Trigger | Effect | Flag |
|---|-----------|---------|--------|------|
| 1 | **Boot-time verify** | daemon start, then every manifest poll | refuse to serve jobs on a runner that fails its SHA check or is below the manifest floor; fetch the current one (verified) first | `self_heal.enabled` |
| 2 | **Zero-work detector + circuit breaker** | runner exit | count registered→exited-without-a-job; after K in a row on a platform, open the breaker (stop provisioning), mark the pool degraded, alert | `self_heal.enabled` |
| 3 | **Break-glass verified auto-upgrade** | breaker opens, or routine poll finds embedded < floor | host daemon fetches the current runner, verifies it against **two independent sources**, swaps it in content-addressed storage, alerts loudly, opens a PR to make the pin permanent | `self_heal.auto_upgrade` (requires 1) |

Mechanism 3 is a bridge: it keeps the fleet alive until a properly pinned
ephemerd release rolls out. It is not a replacement for the pin.

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

The embedded public key placeholder ships empty in this increment. Until a
real key is generated (`minisign -G -p runner-manifest.pub -s <offline>`)
and committed, `EmbeddedPublicKey()` returns an error and every verify
fails closed. The signing key never lives on a node or in CI; manifests
are signed by an operator and uploaded.

`[runner.self_heal] public_key` can override the embedded key (a single
base64 line, as in the `.pub` file). It exists for key rotation and
testing, not as the normal path — a node whose config can be edited can
already be made to run anything.

## Boot-time verify flow

Runs in `cmd/ephemerd serve` **before** the scheduler accepts work, and
again on every manifest poll (default hourly). Host-side only; the
in-VM worker (`--containerd-only`) never does this — it receives the
runner directory from the host as today.

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

"Below floor with auto-upgrade off" cordons the scheduler rather than
thrashing: provisioning runners that will be refused costs GitHub API
quota, a JIT registration per attempt, a container per attempt, and
produces no work. Cordon is what `ephemerd drain` already uses; a node in
that state shows `cordoned=true` on `/healthz` and the alert says why.

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

The detector needs the job-started signal to be *observable*. In
polling mode (no webhooks) and for providers that do not report runner
names (`providers.RunnerNameReporter` false) the detector is disabled for
that provider's runners, the same restriction the orphan sweep already
has — otherwise every short job would count as zero-work.

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
  streak, reason)` once. The wiring (next increment) connects that to the
  same alert channel the disk-full and token-rotation alerts use, and —
  when `auto_upgrade` is on — to the upgrader.

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
it. The integration point is the two `RunnerDir: rm.Dir()` call sites in
`cmd/ephemerd/main.go` (one for the host scheduler, one for the
`--containerd-only` worker), which become `sel.Dir()`. That is a Phase 1
reconciliation and is **not** done in this increment.

## Break-glass auto-upgrade

`[runner.self_heal] auto_upgrade = true` (default `false`; requires
`enabled = true`; host daemon only, never the job VM, never the runner
container). When it is off, `Upgrader.Run` returns `ErrDisabled` before
touching the network.

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

**Known prerequisite.** mayfly never renders `[metrics] bind_addr` into
the node config, so every node's metrics listener binds `127.0.0.1` and
Prometheus scrapes nothing (see the 2026-09 fleet cache audit). Until
that is fixed, these metrics are visible only via `ephemerd metrics` on
the node, and the alerts above must also go through the log-based path.
Fixing the render is a mayfly change outside this subsystem; it is listed
here so nobody designs an alert on a series that is never scraped.

## Configuration

```toml
[runner.self_heal]
enabled = false                # master switch; nothing below runs when false
manifest_url = ""              # https URL of runner-manifest.json; ".minisig" is appended for the signature
public_key = ""                # override the embedded minisign public key (base64 line); rotation/testing only
channel = "stable"             # "stable" | "canary"; manifests for the other channel are ignored
poll_interval = "1h"           # manifest re-check cadence
zero_work_window = "120s"      # registered→self-exit under this with no job-started = zero-work
zero_work_threshold = 3        # consecutive zero-work runners per platform that open the breaker
breaker_cooldown = "15m"       # OPEN → HALF-OPEN
auto_upgrade = false           # break-glass; requires enabled; host daemon only
max_archive_mb = 512           # refuse larger downloads
min_free_gb = 5                # refuse to fetch below this much free space on the cache volume
auto_upgrade_pr_repo = ""      # "ephpm/ephemerd"; empty disables the auto-PR
auto_upgrade_pr_deadline = "72h"
```

Validation (`pkg/config/selfheal.go`): `auto_upgrade` without `enabled`
is an error, `manifest_url` must be `https://`, durations must parse,
`channel` must be one of the two values, threshold ≥ 1.

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
| `in_progress` webhooks stop arriving (webhook outage) | every short runner looks zero-work → breaker opens → **false positive**. The alert text says "or webhook delivery is broken"; the half-open probe reopens it every cooldown. Mitigation (next increment): the detector also treats a provider busy-poll (`RunnerBusy`) as job-started, which the orphan sweep already consults |
| Embedded key placeholder still empty | every verify fails closed; `enabled = true` logs an error at boot naming the missing key; nothing else changes |
| `auto_upgrade` on, breaker open, no manifest at all | nothing happens except the alert — no manifest, no upgrade |

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
- **Rollback**: a pointer swap between two already-verified blobs; no new
  bytes.
- **Auto-PR**: proposes the exact hashes that were verified; a human
  merges; the next release re-verifies at build time. The daemon never
  edits its own pin.

At no point does any path run a runner binary whose SHA-256 was not
checked against a value ephemerd trusted before the bytes arrived.

## What is implemented in this increment

- `pkg/runnerversion/manifest.go` — `Manifest`, `Parse`, `Validate`,
  `PlatformKey`.
- `pkg/runnerversion/minisign.go` — minisign public-key and signature
  parsing, `Verify` (Ed25519 + BLAKE2b-512 prehash, global signature,
  key-ID check); `keys/runner-manifest.pub` placeholder + `EmbeddedPublicKey`.
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
- `pkg/runner/version.go` — **new file**: exported `ParseVersion`,
  `CompareVersions`, `EmbeddedArchive`, `ExtractArchive` wrappers around
  the existing unexported helpers. No existing function in `pkg/runner`
  is modified.
- `pkg/metrics/runnerversion.go` — the four metrics.
- `pkg/config/selfheal.go` — `[runner.self_heal]`, defaults, validation.
  One field added to `RunnerConfig`.

Not wired: nothing in `cmd/ephemerd` or `pkg/scheduler` calls any of it.
The next increment is (1) `Selector` in front of the two `rm.Dir()` call
sites, (2) `Detector.Registered/JobStarted/Exited` from `trackRunning`,
`handleInProgress` and the two wait-goroutines, (3) `Allow(platform)` in
`admitDispatch`, (4) the manifest poll loop and alert routing, (5) the
auto-PR.
