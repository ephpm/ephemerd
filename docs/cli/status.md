---
title: status
weight: 7
---

Show the running daemon's health and job status by querying its gRPC control socket.

```
ephemerd status
```

## Output

Returns a JSON object with the following fields:

| Field | Description |
|-------|-------------|
| `status` | Current daemon status |
| `active_jobs` | Number of jobs currently **tracked** as running |
| `max_concurrent` | Maximum concurrent jobs allowed |
| `held_slots` | Concurrency slots currently held across all dispatch pools |
| `slot_capacity` | Total slots across all dispatch pools |
| `slots` | Per-pool breakdown (`local`, `linux`, `macos`) of `held` and `capacity` |
| `draining` | Whether the daemon is draining (shutting down gracefully) |
| `uptime` | How long the daemon has been running |

### Example output

```json
{
  "status": "running",
  "active_jobs": 2,
  "max_concurrent": 4,
  "held_slots": 2,
  "slot_capacity": 9,
  "slots": [
    { "pool": "local", "held": 2, "capacity": 4 },
    { "pool": "linux", "held": 0, "capacity": 4 },
    { "pool": "macos", "held": 0, "capacity": 1 }
  ],
  "draining": false,
  "uptime": "3h42m15s"
}
```

### held_slots vs active_jobs

They are not the same number, and the difference is diagnostic.

A dispatch takes a concurrency slot *before* it registers the runner and gives
it back *after* the job is untracked, so a job that is still provisioning is
counted in `held_slots` and not in `active_jobs`. Briefly, that is normal.

Sustained, it is not. A pool sitting at `held == capacity` with `active_jobs`
at 0 means the capacity is charged to something the scheduler is not tracking —
a provision that is stuck, or a slot that leaked. The daemon also logs
`waiting for a free concurrency slot` (and escalates to a suspected-leak error)
when a job blocks on such a pool, so the log and this output agree.

The same fields are on `/healthz`.

## Connection

The command connects to the daemon's gRPC unix socket at `<data-dir>/ephemerd.sock`. If the daemon is not running or the socket does not exist, the command prints an error.
