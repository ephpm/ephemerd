---
title: logs
weight: 6
---

Tail the ephemerd system service logs.

```
ephemerd logs [flags]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--lines` | `100` | Number of lines to show |
| `--follow`, `-f` | `false` | Follow log output (stream new entries) |

## Examples

```bash
# Show last 100 lines
ephemerd logs

# Show last 500 lines
ephemerd logs --lines 500

# Follow logs in real time
ephemerd logs -f

# Show last 50 lines then follow
ephemerd logs --lines 50 -f
```

## Platform behavior

| Platform | Command |
|----------|---------|
| Linux | `journalctl -u ephemerd -n <lines> --no-pager` (with `-f` for follow) |
| macOS | `log show --predicate 'subsystem == "dev.ephpm.ephemerd"'` (or `log stream` with `--follow`) |
| Windows | `wevtutil.exe qe Application` filtered to ephemerd provider |
