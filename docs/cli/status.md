# ephemerd status

Shows the current state of the running daemon — active jobs, health, uptime, and whether the scheduler is draining.

## Usage

```
ephemerd status
```

## What it shows

- **Status** — ok or error
- **Active jobs** — number of currently running jobs
- **Max concurrent** — configured concurrency limit
- **Draining** — whether the daemon is shutting down and rejecting new jobs
- **Uptime** — how long the daemon has been running

## How it works

Connects to the daemon's gRPC control socket at `<data-dir>/ephemerd.sock` and calls the `Status` RPC. If the daemon isn't running, the command fails with a connection error.
