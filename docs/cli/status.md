---
title: status
weight: 4
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
| `active_jobs` | Number of jobs currently running |
| `max_concurrent` | Maximum concurrent jobs allowed |
| `draining` | Whether the daemon is draining (shutting down gracefully) |
| `uptime` | How long the daemon has been running |

### Example output

```json
{
  "status": "running",
  "active_jobs": 2,
  "max_concurrent": 4,
  "draining": false,
  "uptime": "3h42m15s"
}
```

## Connection

The command connects to the daemon's gRPC unix socket at `<data-dir>/ephemerd.sock`. If the daemon is not running or the socket does not exist, the command prints an error.
