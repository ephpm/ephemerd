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
| macOS | Tails the launchd log file `/var/log/ephemerd.log` directly (reads the last N lines, then polls for new data when `--follow` is set) |
| Windows | `Get-Content -Path <data-dir>\ephemerd.log -Tail <lines>` via PowerShell (adds `-Wait` for follow) |
