# ephemerd start / stop / restart / logs

Manage the ephemerd system service. These commands wrap the OS-specific service manager so you don't need to remember `systemctl`, `launchctl`, or `sc.exe`.

## Usage

```
ephemerd start          Start the service
ephemerd stop           Stop the service
ephemerd restart        Restart the service
ephemerd logs           Show recent logs (last 100 lines)
ephemerd logs -f        Follow log output in real-time
ephemerd logs --lines 500  Show more lines
```

## Prerequisites

Run `ephemerd install` first to register the system service. These commands operate on the installed service — they don't start ephemerd in the foreground (use `ephemerd serve` for that).

## Platform details

| OS | Service manager | Log source |
|----|----------------|------------|
| Linux | `systemctl start/stop ephemerd` | `journalctl -u ephemerd` |
| macOS | `launchctl load/unload` the LaunchDaemon plist | `log show/stream` with subsystem filter |
| Windows | `sc.exe start/stop ephemerd` | `wevtutil.exe` Application event log |

## Examples

```bash
# Install, start, and follow logs
sudo ephemerd install
sudo ephemerd start
sudo ephemerd logs -f

# Restart after config change
sudo ephemerd restart

# Stop for maintenance
sudo ephemerd stop
```
