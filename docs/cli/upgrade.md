---
title: upgrade
weight: 15
---

Tell the running daemon to upgrade itself to a specific release, then restart into it.

```
ephemerd upgrade --version vX.Y.Z [flags]
```

The CLI does not download anything. It sends a small "go to `vX.Y.Z`" command over the control socket; the **daemon** fetches the release asset over its own outbound HTTPS, checksum-verifies it, drains running jobs, swaps its binary, and hands a restart to the service manager. The CLI streams progress and then polls until the new version is live.

A version is **required**. ephemerd never resolves "latest" on its own -- on a fleet, an implicit "latest" means one bad release can take down every node at once.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--version` | *(required)* | Target release tag to install, e.g. `v0.1.7`. Must look like `vX.Y.Z` unless `--url` is set |
| `--url` | (none) | Override the release base URL for testing or an air-gapped mirror. The asset and `checksums.txt` are fetched from `<url>/` |
| `--no-drain` | `false` | Skip draining before the swap. **Interrupts in-flight jobs** |
| `--force` | `false` | Re-install even if the daemon already reports the target version |
| `--drain-timeout` | `45m` | Give up if running jobs do not finish in time. The upgrade aborts and the node stays on the old binary |
| `--restart-timeout` | `5m` | How long to wait after `restarting` for the new version to come up |

## What the daemon does

Progress is streamed as labelled states:

1. `preflight` -- checks the request; reports `up-to-date` and stops if the daemon already runs the target (unless `--force`).
2. `draining` -- cordons the scheduler and waits for the active job count to reach zero, bounded by `--drain-timeout`. This is the same cordon [`drain --wait`](drain) uses, so you do not need to drain first.
3. `downloading` -- fetches the release asset (byte progress is printed when the server reports a size).
4. `verifying` -- checks the asset against `checksums.txt`.
5. `staging` / `swapping` -- installs the new binary, keeping the previous one beside it as `<name>.old` for rollback. The canonical path and the Windows service `binPath` do not change.
6. `restarting` -- the RPC returns *before* the detached restart fires, so the stream ends cleanly rather than as a broken pipe. The CLI then reconnects and polls `Status` every 3 seconds until the daemon reports the target version.

```bash
$ ephemerd upgrade --version v0.1.7
[preflight] daemon is running v0.1.6, target v0.1.7
[draining] waiting for 2 active job(s)
[downloading] fetching release asset (61%, 640000000/1048576000 bytes)
[verifying] checksum ok
[swapping] installed v0.1.7 (previous kept as ephemerd.old)
[restarting] handing restart to the service manager
Service is restarting into v0.1.7; waiting for it to come up...
Upgraded: daemon is running v0.1.7.
```

## When the restart does not land

Handing a restart to the service manager is not the same as being restarted. If `--restart-timeout` elapses without the new version appearing, the command exits non-zero and spells out the state the node is actually in: the new binary **is** installed on disk, the old one is beside it as `.old`, and the daemon is or is not still accepting jobs.

There is deliberately **no auto-rollback**. The new binary is the component that was checksum-verified and version-probed; what failed is the service-manager hand-off, which a rollback does not fix -- and a rollback can race a restart still in flight. Leaving the verified binary staged means the next restart, automatic or manual, completes the upgrade.

The cordon never outlives a failed upgrade: if the daemon sees that the restart did not take effect, it un-cordons itself and logs the failure loudly, so a node cannot end up drained *and* silently running old code.

## Notes

- The daemon must be running and reachable on the control socket.
- `--no-drain` is for a node you know is idle, or an emergency. It kills in-flight jobs.
- On Windows the restart is driven by a detached `__restart-service` helper spawned from the `.old` backup, which talks to the Service Control Manager directly (stop, wait for STOPPED, start).

## See also

- [drain](drain) -- drain without upgrading
- [uncordon](uncordon) -- clear a cordon left behind by an aborted upgrade
- [status](status) -- reports the running version
