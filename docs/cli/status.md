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
| `abandoned_macos_vms` | macOS VMs whose teardown the daemon gave up on and has not seen die |
| `abandoned_macos_vm_cap` | Value of `abandoned_macos_vms` at which the macOS pool stops taking work |
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
  "abandoned_macos_vms": 0,
  "abandoned_macos_vm_cap": 2,
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

### abandoned_macos_vms

When a macOS VM's `Stop()` does not return — Virtualization.framework can wedge
a teardown indefinitely — the daemon walks away from it after a grace period so
the concurrency slot comes back. The slot is the thing that matters: a held one
costs the node its whole macOS capacity.

The guest, however, is still running, and still holding its full configured RAM.
Nothing in the daemon can reclaim it; only a restart can. So each abandonment is
counted here, and once `abandoned_macos_vms` reaches `abandoned_macos_vm_cap` the
macOS pool refuses to provision anything further and says so at `ERROR` — a
bounded, legible failure instead of unbounded memory growth on a host that
Virtualization.framework will soon refuse anyway.

Non-zero is worth investigating. At the cap, restart the daemon.

`/healthz` carries the same fields, plus `abandoned_macos_vm_details` (job id,
VM instance id, reason and age per stranded guest) when there are any.

## Connection

The command connects to the daemon's gRPC unix socket at `<data-dir>/ephemerd.sock`. If the daemon is not running or the socket does not exist, the command prints an error.
