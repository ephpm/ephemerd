---
title: drain
weight: 8
---

Gracefully drain the running ephemerd daemon. The daemon stops accepting new jobs and waits for all running jobs to finish before exiting.

```
ephemerd drain
```

## Behavior

1. **Read PID file** -- reads `<data-dir>/ephemerd.pid` to find the running daemon's process ID.
2. **Query status** -- if the gRPC control socket is reachable, prints the current number of active jobs.
3. **Send SIGTERM** -- signals the daemon process. The daemon's signal handler then initiates graceful shutdown.

After sending the signal, the command exits immediately. The daemon continues running in the background until all active jobs complete. Use `ephemerd status` to monitor progress.

## Example

```bash
$ ephemerd drain
Active jobs: 3
Sending SIGTERM to ephemerd (pid 12345)...
The daemon will wait for running jobs to finish before exiting.
Use 'ephemerd status' to monitor progress.
```

## Notes

- If the PID file does not exist, the command fails with an error indicating the daemon may not be running.
- This command does not forcefully kill the daemon. To force an immediate stop, send `SIGKILL` directly or use `ephemerd stop`.
