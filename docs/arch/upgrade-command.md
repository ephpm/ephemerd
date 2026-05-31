# `ephemerd upgrade`: In-Place Binary Update

> **Status: proposal.** Not implemented. Scoping document — design,
> tradeoffs, and a work breakdown. Cost estimates are based on adjacent
> tooling (Tailscale's `tailscale update`, k0s `k0sctl apply`, Docker
> CE upgrade flows) and are best-guess until prototyped.

## Context

Today, updating ephemerd on a host is a manual five-step ritual:

1. `git pull` on the host (or copy a fresh tree).
2. `mage build:windows` (or `:macos` / current-OS variant), about 5
   minutes including the embedded Linux cross-compile.
3. `ephemerd stop`, wait for the process to actually exit (Windows
   service shutdown races; binary stays locked for a beat).
4. Copy the new binary to `C:\Program Files\ephemerd\ephemerd.exe`
   (or `/usr/local/bin/ephemerd` on Linux/macOS).
5. `ephemerd start`, poll for the in-VM ephemerd to come back up,
   `grep` the console log to confirm the version baked in is the one
   we just built.

That works for one host. For three weekly iterations on one host it
becomes annoying. For a fleet — multiple hosts per org, plus the
~half-dozen test rigs the team would want to keep current — it doesn't
scale.

The dind work in PRs #82–#85 also surfaced that "is the new code
actually running" is a non-obvious question. The Windows daemon and the
in-VM ephemerd are *two* binaries (the Linux one is embedded into the
Windows one and extracted on every VM boot), and a stale build can run
silently if the deploy missed either layer. An upgrade command should
make that uncertainty go away by handling both halves and reporting the
resulting version end-to-end.

## Goals

1. **One command per host.** `ephemerd upgrade` does the entire update.
2. **No source tree required on the target.** Hosts that aren't dev
   workstations shouldn't need Go, mage, the repo, or 5 minutes of CPU
   to update.
3. **Per-channel pinning.** A prod host configured for `stable` can't
   accidentally pull a `main` build. A dev host can opt in.
4. **Drain-safe.** No in-flight jobs get killed by an upgrade.
5. **Rollback-safe.** Failed startup of the new binary rolls back to
   the previous one automatically.
6. **End-to-end version reporting.** Post-upgrade output names *both*
   the Windows-daemon version and the in-VM ephemerd version, so the
   "I deployed, the fix didn't take" story from the dind work can't
   recur silently.

Non-goals: rolling fleet upgrades (one host at a time is fine; multi-host
orchestration is a layer above this), zero-downtime (drain + restart is
acceptable for our SLA), self-updating from arbitrary URLs (channels
only).

## Design

### Artifact source

Pre-built binaries published by CI on every push to main and on every
release tag. The simplest store is GitHub Releases:

- **`stable` channel** → latest tag matching `v*.*.*`, downloaded from
  that release's assets.
- **`main` channel** → a rolling release named `latest-main`, updated
  by CI on every push to `main`. Same asset layout as a tagged release.
- **`pinned` channel** → `--tag vX.Y.Z` for one-shot updates to a
  specific version; also settable in config.

Each release publishes:

```
ephemerd-windows-amd64.exe       (~880 MB — embeds linux binary)
ephemerd-linux-amd64             (~240 MB)
ephemerd-linux-arm64             (~240 MB)
ephemerd-darwin-arm64            (~similar — embeds Vz linux assets)
SHA256SUMS                       (signed)
SHA256SUMS.asc                   (detached signature — optional v1)
```

The upgrade command picks the asset matching its host's GOOS/GOARCH.

Tradeoff: GitHub Releases is free and integrates trivially with our
existing CI, but downloads are rate-limited and unauthenticated pulls
get throttled aggressively. Anonymous pulls from a busy fleet may hit
the limit; authenticated pulls (using the host's `GITHUB_TOKEN`)
sidestep it. For v1 we rely on the auth token ephemerd already holds.

### Channel config

```toml
# /etc/ephemerd/config.toml (or %ProgramData%\ephemerd\config.toml)
[upgrade]
channel       = "stable"        # "stable" | "main" | "pinned"
pinned_tag    = ""              # only used when channel = "pinned"
auto_check    = true            # poll for new versions periodically
check_interval = "24h"          # how often to log "newer version available"
```

Default is `stable`. A fresh install can't accidentally float into
`main` without an explicit config change.

### Command shape

```
ephemerd upgrade [flags]
  --channel <stable|main|pinned>   override config channel for this run
  --tag <vX.Y.Z>                   shorthand for --channel pinned --pinned-tag
  --check                          report available version, don't upgrade
  --dry-run                        show what would happen, don't do it
  --force                          skip version check (re-deploy current)
  --no-drain                       skip drain (operator override)
```

Default flow (no flags):

1. Resolve channel → download URL → expected version.
2. `--check` returns here.
3. Compare to running version; no-op if equal (unless `--force`).
4. Download artifact + SHA256 manifest to `<install-dir>/.upgrade/`.
5. Verify SHA256. (GPG/cosign optional — v2.)
6. Pre-flight: confirm we have permission to swap the binary,
   service-manager access, etc.
7. **Drain** the daemon — refuse new jobs, wait for active jobs to
   exit (configurable timeout; default 30 min, surface via flag).
8. `ephemerd stop`, wait for process to truly exit.
9. Move current binary to `<install-dir>/.upgrade/ephemerd.previous`,
   move new binary into place.
10. `ephemerd start`. Poll `ephemerd status` for "ok" within 60s.
11. Wait for in-VM ephemerd to log its version (parse console.log on
    Windows; equivalent on macOS).
12. Report: `upgraded host:vA.B.C -> vX.Y.Z, in-vm:vX.Y.Z`.
13. On any failure between step 9 and 12, swap `.previous` back, restart,
    log the rollback, exit non-zero.

### Drain mechanics

`ephemerd drain` is broken on Windows today (per project memory:
SIGTERM not supported). The upgrade work needs to fix that anyway —
options:

- Add a `Drain` RPC to the gRPC control API (`api/v1/`). The CLI calls
  it; the scheduler flips a flag that rejects new jobs and waits for
  active ones to exit. Cross-platform, doesn't depend on signal
  handling. Probably the right answer.
- Or: replace SIGTERM with a Windows service-control event
  (`SERVICE_CONTROL_PARAMCHANGE` or a custom code). Less invasive but
  Windows-specific.

Recommendation: RPC. Reusable for the existing `ephemerd drain`
command, which would also become a thin wrapper around the same call.

### Atomic swap mechanics, per OS

**Linux/macOS**: `rename(2)` can replace a running executable's file.
Open file handles keep pointing at the old inode until the process
exits, the new inode takes the path immediately. The systemd/launchd
restart picks up the new binary.

**Windows**: can't replace a locked file. Sequence has to be:

```
ephemerd stop           (via service-control)
wait for process exit
copy/move new binary
ephemerd start          (via service-control)
```

The five-second window where the service is fully down is acceptable
because we drained first. The CLI orchestrates via the Windows Service
Manager API (already used by `ephemerd start/stop`).

### Version reporting end-to-end

Post-upgrade, the command output should look like:

```
$ ephemerd upgrade
Channel: stable
Current host binary:   v1.4.2 (built 2026-05-30)
Available:             v1.4.3 (released 2026-06-02)
Draining... 0 active jobs.
Stopping service... done (1.2s).
Replacing binary at C:\Program Files\ephemerd\ephemerd.exe... done.
Starting service... ok (3.4s).
Waiting for in-VM ephemerd to register... ok.

Upgraded:
  host binary:   v1.4.2 -> v1.4.3
  in-VM binary:  v1.4.2 -> v1.4.3
```

The in-VM version comes from parsing the first "starting ephemerd"
line in `<DataDir>/vm/linux/console.log` after the restart (or the
equivalent on macOS / Vz).

### Self-replacement detail

The upgrade command is itself part of the binary being replaced. On
Linux/macOS this is fine (open inode survives). On Windows the running
`ephemerd upgrade` process holds the lock on the daemon binary only
indirectly (the running service does), so the CLI can swap freely
after stopping the service. The CLI also needs to NOT delete itself
if it's running from the same install path — handle that by either:

- Running from a temp copy (CLI's first act is to `exec` itself from
  a tempdir, then proceed).
- Or scoping `upgrade` to be invoked from a separate path
  (`ephemerd-upgrader.exe` or just `ephemerd upgrade --from <path>`).

The temp-copy approach is the standard pattern (Tailscale, Docker
Desktop, vscode auto-update all do it). Cleaner for the user.

## CI work

The biggest unknown — the upgrade CLI is straightforward, the artifact
publishing is where the time goes.

Required:

1. **Release workflow** (`.github/workflows/release.yml`):
   - Triggers: `push: tags: ['v*']` and `workflow_dispatch`.
   - Matrix: linux/amd64, linux/arm64, windows/amd64, darwin/arm64.
   - Runs `mage build:<os>` per cell, uploads as a release asset.
   - Generates `SHA256SUMS` from all assets.

2. **Rolling-main workflow** (`.github/workflows/main-release.yml`):
   - Triggers: `push: branches: [main]`.
   - Same matrix, same artifacts.
   - Publishes to a single GitHub Release tagged `latest-main` (move-tag
     pattern: delete the tag, retag HEAD, recreate the release with
     fresh assets).

3. **Signing** (deferred to v2 unless we already have a code-signing cert):
   - Windows: Authenticode (cert + EV recommended; ~$300/year).
   - macOS: notarization via `notarytool` (free with an Apple Developer
     account; ad-hoc signing already in place per memory).
   - Linux: optional GPG signature on SHA256SUMS.

Pragmatic v1: SHA256 checksum only. Signing comes later.

### Storage cost

Each release ≈ 1.8 GB of binaries (4 platforms × ~0.5 GB average). GitHub
Releases storage is free but assets count against the 2 GB/file limit
(we're fine, biggest is ~880 MB). With a tagged release per week plus
a constantly-updated `latest-main`, expect ~10 GB of active artifact
storage; well within limits.

## Risks

- **GitHub rate limits on download.** Mitigated by authenticated pulls.
  If we move off GitHub later (S3, an OCI registry), the upgrade CLI's
  download layer is the only thing that changes.
- **Auto-check noise.** A daemon that logs "newer version available"
  every 24h gets ignored. Make it opt-in or surface in `ephemerd status`
  instead of the running log.
- **Drain that never completes.** A hung job blocks the upgrade
  indefinitely. Default 30-minute drain timeout with a clear "still
  running: <job-id>" message before timeout; `--force` skips drain
  entirely for emergency upgrades.
- **In-VM version mismatch detection.** The current "grep console.log
  for `starting ephemerd version=`" is fragile. A more durable
  solution: the in-VM ephemerd exposes its version via the gRPC
  control API; the upgrade command queries the in-VM dispatch RPC
  directly. That's a separate small piece of work.
- **Channel drift.** A host configured for `stable` could be tricked
  via `--channel main` flag. Acceptable — operator-explicit override is
  fine. The lock is against passive drift, not against the operator.
- **Cross-version compatibility.** Schema changes (BoltDB, gRPC API)
  during a rolling fleet upgrade could break older nodes pointing at
  newer schedulers. ephemerd is single-host today so this isn't an
  issue, but worth flagging if multi-host coordination ever happens.

## Estimate

Rough sizing, assuming one engineer focused:

| Piece | Effort | Notes |
|---|---|---|
| CI release workflow (tags) | 1d | One matrix, four mage targets. |
| CI rolling-main workflow | 0.5d | Tag-move + asset-replace dance. |
| Drain RPC | 1d | gRPC method + scheduler hook + Windows fix. |
| Upgrade CLI: download + verify | 1d | Channel resolution, SHA256, retry. |
| Upgrade CLI: swap + restart | 1.5d | Service-manager glue per OS, rollback, self-exec from tempdir. |
| Upgrade CLI: version reporting | 0.5d | Parse console.log on Windows, equivalent elsewhere; better with in-VM RPC. |
| Tests | 1.5d | Unit + e2e: fake artifact server, version-mismatch rollback, drain-timeout. |
| Docs | 0.5d | CLI reference, configuration reference, ops guide. |

**Total: ~7 engineer-days for a solid v1.** Could ship a "happy-path
only, manual rollback" version in ~3 days as a stopgap.

Signing/notarization adds another 2-3 days each, deferred unless
distribution policy demands it.

## Open questions

1. **In-VM version source of truth.** Parse console.log (simple, works
   today) vs add a gRPC call to the in-VM dispatch server (more
   robust, requires the dispatch server to be reachable post-restart).
   Recommend the gRPC call — it's small and we already have the
   dispatch service.
2. **Auto-upgrade.** Should ephemerd ever upgrade itself without an
   operator running the command? Pro: zero-touch fleet. Con: the dind
   debugging we just did would have been *much* harder if the daemon
   silently rolled forward overnight. Recommend: never auto-apply,
   only auto-check + log.
3. **Multi-channel hosts.** Can the same host run two ephemerd
   instances on different channels (e.g. for A/B testing)? Probably
   no for v1; one binary per host. Revisit if needed.
4. **Downgrade.** `ephemerd upgrade --tag <older>` should work for
   rollbacks. Worth explicit testing.
5. **Embedded asset version skew.** A v1.4.3 host binary embeds a
   v1.4.3 Linux binary. If the embed somehow gets stale (cache bug
   like the one we hit during the dind work), the post-upgrade version
   report should *catch the mismatch* — log a WARN and fail the
   upgrade. That alone would have saved several hours.

## Recommendation

Build the CI artifact pipeline first (it's the bottleneck and unblocks
everything else), then the CLI in a single PR, then the drain RPC fix
as a small follow-up. Ship `--check` + manual download as a stopgap
on day 3 so the team has *something* before the full automation lands.
