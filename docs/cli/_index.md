---
title: CLI Reference
weight: 3
cascade:
  type: docs
---

Complete reference for all ephemerd commands.

The `ephemerd` binary uses `serve` as its default command. All commands accept the global `--data-dir` flag to override the data directory (default: `/var/lib/ephemerd` on Linux/macOS, `C:\ProgramData\ephemerd` on Windows). The `EPHEMERD_DATA_DIR` environment variable can also set this.

| Command | Description |
|---------|-------------|
| [serve](serve) | Start the ephemerd daemon (default command) |
| [run](run) | Run a GitHub Actions workflow locally |
| [start](service) | Start the ephemerd system service |
| [stop](service) | Stop the ephemerd system service |
| [restart](service) | Restart the ephemerd system service |
| [logs](service) | Tail the ephemerd system service logs |
| [status](status) | Show running jobs and daemon health |
| [drain](drain) | Gracefully drain the running daemon |
| [jobs](jobs) | List and manage running jobs |
| [images](images) | List cached container images |
| [config](config) | Validate configuration file |
| [doctor](doctor) | Check system readiness and clean up stale state |
| [install](install) | Install ephemerd as a system service |
| [uninstall](uninstall) | Remove ephemerd binary, service, and data |
| [ctrctl](ctrctl) | Passthrough to containerd's ctr CLI |
| crictl | Passthrough to containerd's CRI interface (in-process crictl) |
