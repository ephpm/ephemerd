---
title: uninstall
weight: 14
---

Remove the ephemerd binary, system service, and optionally all data.

```
ephemerd uninstall [flags]
```

## Flags

| Flag | Description |
|------|-------------|
| `--keep-data` | Keep the data directory (config, logs, container state) |

## Steps

### 1. Clean up runtime state

Runs the same cleanup as `ephemerd doctor --clean` to remove stale containers, network bridges, WSL distros, and other runtime state before removing the data directory.

### 2. Stop and remove the service

Platform-specific service removal:

- **Linux**: `systemctl stop ephemerd`, `systemctl disable ephemerd`, removes `/etc/systemd/system/ephemerd.service`, runs `systemctl daemon-reload`.
- **macOS**: `launchctl unload /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`, removes the plist file.
- **Windows**: `sc.exe stop ephemerd`, `sc.exe delete ephemerd`.

### 3. Remove the binary

Removes the ephemerd binary from its installed location. On Windows, the running binary cannot be deleted -- a message is printed asking you to delete it manually after the process exits.

### 4. Remove the data directory

Unless `--keep-data` is set, removes the entire data directory (`/var/lib/ephemerd` or `C:\ProgramData\ephemerd`) including config, logs, and all container state.

Also removes environment files at `/etc/default/ephemerd` and `/etc/sysconfig/ephemerd` if they exist.

## Examples

```bash
# Full uninstall (remove everything)
sudo ephemerd uninstall

# Uninstall but keep config and logs
sudo ephemerd uninstall --keep-data
```
