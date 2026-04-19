# ephemerd uninstall

Completely removes ephemerd from the system — binary, service, and all data.

## Usage

```
ephemerd uninstall [--keep-data]
```

## Flags

- `--keep-data` — preserve the data directory (config, logs, container state). Useful if you plan to reinstall.

## What it removes

### 1. Runtime state cleanup

Before removing files, uninstall runs the same cleanup as `ephemerd doctor --clean`:

- Stale control socket (`<data-dir>/ephemerd.sock`)
- Stale PID file (`<data-dir>/ephemerd.pid`)
- **Linux:** stale CNI state, DNS config, `ephemerd0` network bridge
- **Windows:** stale `ephemerd-*` WSL distros, VM directories
- **macOS:** stale VM clone directories

### 2. System service

- **Linux:** stops and disables `ephemerd.service`, removes `/etc/systemd/system/ephemerd.service`, runs `systemctl daemon-reload`
- **macOS:** unloads and removes `/Library/LaunchDaemons/dev.ephpm.ephemerd.plist`
- **Windows:** stops and deletes the `ephemerd` Windows service via `sc.exe`

### 3. Binary

- Removes the `ephemerd` binary from its installed location (resolved via `os.Executable()`)
- On Windows, the binary can't be deleted while running — prints a message to delete manually after exit

### 4. Data directory (unless --keep-data)

- Removes the entire data directory (default `/var/lib/ephemerd` on Linux, `C:\ProgramData\ephemerd` on Windows)
- This includes: config file, job logs, containerd state, cached images, extracted binaries

### 5. Environment files (unless --keep-data)

- `/etc/default/ephemerd` — systemd environment file containing `GITHUB_TOKEN`
- `/etc/sysconfig/ephemerd` — alternative location (RHEL/CentOS)

## Reinstalling after uninstall

If you used `--keep-data`, your config and logs are preserved. Just run the install script again:

```bash
curl -fsSL https://raw.githubusercontent.com/ephpm/ephemerd/main/install.sh | sudo bash
```

The installer will detect the existing config and skip creating a new one.
