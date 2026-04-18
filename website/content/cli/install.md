---
title: "^# "
---


Install ephemerd as a system service. Downloads nothing — uses the binary you already have.

## Usage

```
ephemerd install [--no-service]
```

## Flags

- `--no-service` — skip service registration (just copy binary and create config)

## What it does

### 1. Copy binary

Copies itself to the system install directory:
- **Linux/macOS:** `/usr/local/bin/ephemerd`
- **Windows:** `C:\Program Files\ephemerd\ephemerd.exe`

Skipped if already running from the install location.

### 2. Create default config

Creates `<data-dir>/config.toml` with sensible defaults if it doesn't exist. Does not overwrite existing config files.

Default data directory:
- **Linux:** `/var/lib/ephemerd`
- **macOS:** `/var/lib/ephemerd`
- **Windows:** `C:\ProgramData\ephemerd`

### 3. Register system service

**Linux (systemd):**
- Creates `/etc/systemd/system/ephemerd.service`
- Runs `systemctl daemon-reload`
- Creates `/etc/default/ephemerd` for environment variables (GITHUB_TOKEN)
- Service is configured with `Restart=on-failure` and `KillMode=mixed` for graceful shutdown

**macOS (launchd):**
- Creates `/Library/LaunchDaemons/dev.ephpm.ephemerd.plist`
- Configured with `KeepAlive` and `RunAtLoad`
- Logs to `/var/log/ephemerd.log`

**Windows (sc.exe):**
- Creates a Windows service named `ephemerd` with delayed auto-start
- Configures restart-on-failure recovery (5 second delay)
- Creates `<data-dir>\env.cmd` for environment variable reference

## After installing

1. Edit the config file (set `github.owner`)
2. Set `GITHUB_TOKEN` in the environment file
3. Start the service:
   - Linux: `sudo systemctl start ephemerd && sudo systemctl enable ephemerd`
   - macOS: `sudo launchctl load /Library/LaunchDaemons/dev.ephpm.ephemerd.plist`
   - Windows: `sc.exe start ephemerd`
