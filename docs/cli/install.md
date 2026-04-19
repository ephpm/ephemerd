---
title: install
weight: 13
---

Install ephemerd as a system service. Copies the binary to the system install location, creates the data directory with a default config, and registers a system service.

```
ephemerd install [flags]
```

## Flags

| Flag | Description |
|------|-------------|
| `--no-service` | Skip service registration (only copy binary and create config) |

## Steps

### 1. Copy binary

Copies the running ephemerd binary to the platform's install directory:

| Platform | Install path |
|----------|-------------|
| Linux | `/usr/local/bin/ephemerd` |
| macOS | `/usr/local/bin/ephemerd` |
| Windows | `C:\Program Files\ephemerd\ephemerd.exe` |

If the binary is already at the target location, this step is skipped.

### 2. Create data directory and default config

Creates the data directory and writes a default `config.toml` if one does not already exist. The default config contains:

```toml
[github]
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional -- omit for org-level runners

[runner]
max_concurrent = 4

[log]
level = "info"
```

If a config file already exists, it is not overwritten.

### 3. Register system service

Unless `--no-service` is set, registers a platform-specific system service:

#### Linux (systemd)

Creates `/etc/systemd/system/ephemerd.service` with:
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`
- `TimeoutStopSec=300` (5 minutes for graceful drain)
- `KillMode=mixed`
- `EnvironmentFile=-/etc/default/ephemerd` for the GitHub token

Also creates `/etc/default/ephemerd` with a placeholder for `GITHUB_TOKEN`.

#### macOS (launchd)

Creates `/Library/LaunchDaemons/dev.ephpm.ephemerd.plist` with:
- `RunAtLoad` and `KeepAlive` enabled
- Logs written to `/var/log/ephemerd.log`

#### Windows (sc.exe)

Creates a Windows service named `ephemerd` with:
- `start=delayed-auto` (starts automatically after boot)
- Restart-on-failure recovery (5-second delay)
- Service description set

## Post-install steps

After installation, the command prints platform-specific next steps:

**Linux:**
1. Edit `<data-dir>/config.toml` (set `github.owner`)
2. Set `GITHUB_TOKEN` in `/etc/default/ephemerd`
3. `sudo systemctl start ephemerd`
4. `sudo systemctl enable ephemerd`

**macOS:**
1. Edit `<data-dir>/config.toml` (set `github.owner`)
2. Set `GITHUB_TOKEN` in the launchd plist or `/etc/default/ephemerd`
3. `sudo launchctl load /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`

**Windows:**
1. Edit `<data-dir>\config.toml` (set `github.owner`)
2. Set `GITHUB_TOKEN` as a system environment variable
3. `sc.exe start ephemerd`

## Examples

```bash
# Full install (binary + config + service)
sudo ephemerd install

# Install binary and config only (no service)
sudo ephemerd install --no-service
```
