---
title: doctor
weight: 12
---

Check system readiness and clean up stale runtime state. By default, runs both checks and cleanup. Use `--check` or `--clean` to run only one phase.

```
ephemerd doctor [flags]
```

## Flags

| Flag | Description |
|------|-------------|
| `--check` | Run checks only, skip cleanup |
| `--clean` | Run cleanup only, skip checks |

## System checks

### Cross-platform checks

These checks run on all platforms:

- **Config file** -- looks for `config.toml` in the data directory, `/etc/ephemerd/`, or `<data-dir>/ephemerd.toml`. Validates the config if found.
- **GITHUB_TOKEN** -- checks whether the environment variable is set.
- **Data directory** -- verifies the data directory exists and is writable.
- **Disk space** -- checks free disk space. Warns below 20 GB (Linux) or 30 GB (macOS/Windows). Fails below 5 GB (Linux) or 10 GB (macOS/Windows).
- **Embedded assets** -- confirms assets were compiled into the binary at build time.

### Linux checks

- **iptables** -- verifies `iptables` is in `PATH`.
- **Kernel namespaces** -- checks for `net`, `pid`, `mnt`, `uts`, and `ipc` namespaces in `/proc/self/ns/`.
- **cgroups** -- detects cgroups v2 (preferred) or v1.
- **Filesystem** -- checks the root filesystem type. Warns if NFS or ZFS (overlayfs not supported on these).
- **Root privileges** -- verifies the process is running as root.

### macOS checks

- **macOS version** -- reports the OS version.
- **Architecture** -- verifies Apple Silicon (arm64). Virtualization.framework requires it.
- **Virtualization entitlement** -- checks the binary's code signature for the `com.apple.security.virtualization` entitlement.
- **macOS VM disk image** -- looks for an existing disk image. If not found, notes that ephemerd will download and install the Apple IPSW on first boot.
- **VM capacity** -- calculates how many concurrent macOS VMs the host can support based on CPU and memory.

### Windows checks

- **WSL** -- checks for `wsl.exe` in `PATH` and verifies WSL2 is available.
- **Hyper-V** -- checks whether the Hyper-V hypervisor feature is enabled.
- **Windows version** -- reports the OS version.
- **Containers feature** -- checks whether the Windows Containers optional feature is enabled.

## Cleanup

Cleanup runs as the second phase (requires `sudo` on Linux/macOS for full cleanup). Operations performed:

- **Orphan containers** -- checks for stale container state directories.
- **Stale network bridge** -- on Linux, removes the `ephemerd0` bridge if it exists.
- **Control socket** -- removes the stale `ephemerd.sock` file.
- **PID file** -- removes the stale `ephemerd.pid` file.
- **Old job logs** -- removes log files older than 7 days from `<data-dir>/logs/`.

### Linux-specific cleanup

- Removes stale CNI state (preserving `bin/` and `conf/` directories).
- Removes stale DNS configuration directory.
- Deletes the `ephemerd0` network bridge if present.

### macOS-specific cleanup

- Removes stale macOS VM clone directories from `<data-dir>/vm/macos/jobs/`.

### Windows-specific cleanup

- Unregisters stale WSL distros matching the `ephemerd-*` prefix.
- Removes stale VM directories (preserving `embed/`).

## Output

```
System checks:

  ✓ config file valid (/var/lib/ephemerd/config.toml)
  ✓ GITHUB_TOKEN is set
  ✓ data directory writable (/var/lib/ephemerd)
  ✓ 85.2 GB free disk space
  ✓ embedded assets compiled in (verified at build time)

Platform checks (linux/amd64):

  ✓ iptables available
  ✓ kernel namespaces available (net, pid, mnt, uts, ipc)
  ✓ cgroups v2 available
  ✓ filesystem supports overlayfs
  ✓ running as root

Cleanup:

  ✓ no orphan containers
  ✓ cleaned stale network bridge (if any)
  ✓ no stale control socket
  ✓ no stale PID file
  ✓ cleaned old job logs (>7 days)
  ✓ cleaned stale CNI state
  ✓ no stale DNS config

Results: 12 passed, 0 warnings, 0 failed
```

The command exits with a non-zero status if any checks fail.

## Examples

```bash
# Run all checks and cleanup
sudo ephemerd doctor

# Check system readiness without modifying anything
ephemerd doctor --check

# Clean up stale state only
sudo ephemerd doctor --clean
```
