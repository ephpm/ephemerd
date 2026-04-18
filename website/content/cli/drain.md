---
title: "^# "
---


Tells the running daemon to stop accepting new jobs and wait for running jobs to finish. Used for graceful maintenance.

## Usage

```
ephemerd drain
```

## What it does

1. Sends SIGTERM to the ephemerd process (found via PID file at `<data-dir>/ephemerd.pid`)
2. The daemon sets `draining = true` — new queued jobs are rejected
3. Running jobs continue until they complete or the shutdown timeout expires (default 5 minutes)
4. After all jobs finish (or timeout), the daemon exits cleanly

## When to use it

- Before system maintenance or reboots
- Before upgrading ephemerd to a new version
- When you want to temporarily stop processing jobs without killing running ones

The daemon exits after draining. To resume, start it again with `ephemerd serve` or `systemctl start ephemerd`.
