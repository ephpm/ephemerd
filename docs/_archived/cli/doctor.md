# ephemerd doctor

Validates that the system has everything needed to run ephemerd and cleans up stale state from crashes or unclean shutdowns.

## Usage

```
ephemerd doctor [--check] [--clean]
```

## Flags

- `--check` — run checks only, skip cleanup
- `--clean` — run cleanup only, skip checks

With no flags, runs both checks and cleanup.

## System checks

### All platforms

- **Config file** — looks for `<data-dir>/config.toml`, validates it parses correctly
- **GitHub token** — checks `GITHUB_TOKEN` environment variable is set
- **Data directory** — verifies the directory is writable
- **Disk space** — warns below 20 GB, fails below 5 GB (10 GB thresholds on Windows/macOS due to larger images)
- **Embedded assets** — verifies the binary was built with embedded runner/CNI/shim assets

### Linux

- **iptables** — required for container network isolation
- **Kernel namespaces** — checks `/proc/self/ns/{net,pid,mnt,uts,ipc}`
- **cgroups** — checks for v2 (recommended), warns on v1
- **Filesystem** — warns if the data directory is on ZFS or NFS (overlayfs not supported, containerd falls back to the native snapshotter which copies full images per container instead of using copy-on-write layers)
- **Root** — ephemerd requires root for container management

### Windows

- **WSL2** — required for Linux jobs on Windows hosts
- **Hyper-V** — required for Windows container isolation
- **Windows Containers feature** — must be enabled
- **Windows build version** — informational

### macOS

- **Apple Silicon** — Virtualization.framework requires arm64
- **Virtualization entitlement** — binary must be code-signed with `com.apple.security.virtualization`
- **Base image** — checks for a macOS VM base image (required for macOS-native jobs)

## Cleanup

### All platforms

- **Stale control socket** — removes `<data-dir>/ephemerd.sock` left by a crashed daemon
- **Stale PID file** — removes `<data-dir>/ephemerd.pid`
- **Old job logs** — removes logs older than 7 days from `<data-dir>/logs/`

### Linux

- **Stale CNI state** — removes IP allocations and network config from previous runs
- **Stale DNS config** — removes `<data-dir>/dns/` directory
- **Stale network bridge** — deletes the `ephemerd0` bridge interface if it exists

### Windows

- **Stale WSL distros** — unregisters any `ephemerd-*` WSL distros left by crashed runs
- **Stale VM directories** — removes `<data-dir>/vm/` subdirectories (except embedded assets)

### macOS

- **Stale VM clones** — removes APFS clone directories from `<data-dir>/vm/macos/clones/`

## Exit codes

- `0` — all checks passed (warnings are OK)
- `1` — one or more checks failed
