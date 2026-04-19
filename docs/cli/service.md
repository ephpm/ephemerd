---
title: Service Management
weight: 3
---

Ephemerd provides four commands for managing the system service: `start`, `stop`, `restart`, and `logs`. Each command delegates to the platform's native service manager.

## start

Start the ephemerd system service.

```
ephemerd start
```

## stop

Stop the ephemerd system service.

```
ephemerd stop
```

## restart

Restart the ephemerd system service. Internally this runs `stop` followed by `start`. If the stop fails (e.g., the service was not running), it prints a note and proceeds with start.

```
ephemerd restart
```

## logs

Tail the ephemerd system service logs.

```
ephemerd logs [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--lines` | `100` | Number of lines to show |
| `--follow`, `-f` | `false` | Follow log output (stream new entries) |

### Examples

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

### Linux (systemd)

- `start` / `stop` / `restart`: runs `systemctl <action> ephemerd`.
- `logs`: runs `journalctl -u ephemerd -n <lines> --no-pager`. With `--follow`, appends `-f`.

### macOS (launchd)

- `start`: runs `launchctl load -w /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`.
- `stop`: runs `launchctl unload /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`.
- `logs` without `--follow`: runs `log show --predicate 'subsystem == "dev.ephpm.ephemerd"'`.
- `logs` with `--follow`: runs `log stream --predicate 'subsystem == "dev.ephpm.ephemerd"'`.

### Windows (sc.exe)

- `start` / `stop`: runs `sc.exe <action> ephemerd`.
- `logs`: runs `wevtutil.exe qe Application` filtered to the `ephemerd` provider, formatted as text.
