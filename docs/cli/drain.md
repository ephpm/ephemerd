---
title: drain
weight: 8
---

Stop the running ephemerd daemon from claiming new jobs, and let the jobs already in flight finish.

```
ephemerd drain [flags]
```

There are two modes. The default **stops the daemon** once its jobs finish. `--wait` **keeps the daemon running** and just blocks until it is idle, so you can restart it yourself without killing a job.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--wait` | `false` | Cordon over the control socket (no signal, daemon keeps running) and block until every running job finishes |
| `--timeout` | `45m` | With `--wait`: give up after this long. Exits non-zero and leaves the daemon cordoned |

## Default mode (drain and stop)

Returns immediately -- the daemon keeps running in the background until its jobs finish. Use `ephemerd status` to watch progress.

**On Linux and macOS**, `drain` reads `<data-dir>/ephemerd.pid`, prints the current active job count if the control socket is reachable, and sends `SIGTERM`. The daemon's signal handler stops claiming, finishes in-flight jobs (or gives up at `shutdown_timeout`, default 5m), then exits.

```bash
$ ephemerd drain
Active jobs: 3
Sending SIGTERM to ephemerd (pid 12345)...
The daemon will wait for running jobs to finish before exiting.
Use 'ephemerd status' to monitor progress.
```

**On Windows** there are no POSIX signals, so the default path does the equivalent over supported interfaces: it cordons the daemon through the control socket, then asks the Service Control Manager to stop the service. The service handler runs the same graceful shutdown and holds `StopPending` while in-flight jobs finish.

```
$ ephemerd drain
Cordoned: daemon stopped claiming new jobs (3 active).
Asked the service manager to stop ephemerd; it exits once running jobs finish.
Use 'ephemerd status' to monitor progress.
```

The cordon deliberately happens first, because it works whether or not ephemerd was installed as a service. If the SCM stop then fails -- running in the foreground, no service registered, insufficient rights -- the command still reports success (the node is claiming nothing) and tells you how to finish by hand.

## `--wait` mode (drain in place)

No signal is sent and the service is not stopped. The daemon is **cordoned**: it stops claiming new jobs but keeps serving the ones it has. The command then polls `Status` every 5 seconds until the active job count hits zero, or `--timeout` elapses.

```bash
$ ephemerd drain --wait
Cordoned: daemon stopped claiming new jobs (2 active).
Waiting: 2 active job(s), elapsed 5s
Waiting: 1 active job(s), elapsed 1m10s
Drained: no active jobs (waited 3m20s).
```

Exit 0 means the node is idle, so a restart at that moment kills nothing:

```bash
ephemerd drain --wait && systemctl restart ephemerd
```

On timeout the command exits non-zero and **leaves the daemon cordoned** on purpose, so you can decide: restart anyway, or run [`uncordon`](uncordon) to resume claiming.

```
drain timed out after 45m0s with 1 job(s) still running (daemon stays cordoned)
```

Interrupting the command (Ctrl-C) also leaves the cordon in place.

## Cordon / uncordon workflow

`--wait` and `uncordon` are the two halves of a maintenance window:

```bash
ephemerd drain --wait        # stop claiming, wait for idle
# ... do the maintenance ...
ephemerd uncordon            # resume claiming
```

A cordon set this way is runtime state on the running daemon, not config. Restarting the daemon clears it.

## Notes

- The default (non-`--wait`) mode on Linux/macOS requires the PID file. If `<data-dir>/ephemerd.pid` is missing, the command fails with an error suggesting the daemon may not be running.
- Neither mode forcefully kills the daemon or its jobs. To force an immediate stop, use `ephemerd stop`.
- `ephemerd upgrade` performs the same wait-for-idle drain internally before swapping the binary; you do not need to drain first. See [upgrade](upgrade).

## See also

- [uncordon](uncordon) -- resume claiming after a cordon or an aborted drain
- [status](status) -- watch the active job count while draining
- [stop](stop) -- stop the service without waiting for jobs
