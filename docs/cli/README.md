# CLI Reference

ephemerd is a single binary with several subcommands.

| Command | Description |
|---|---|
| [serve](serve.md) | Start the daemon in the foreground |
| [run](run.md) | Run a workflow locally without pushing to GitHub |
| [install](install.md) | Install binary and register as a system service |
| [uninstall](uninstall.md) | Remove ephemerd from the system |
| [start](service.md) | Start the ephemerd system service |
| [stop](service.md) | Stop the ephemerd system service |
| [restart](service.md) | Restart the ephemerd system service |
| [logs](service.md) | Tail the ephemerd system service logs |
| [status](status.md) | Show running jobs, health, uptime |
| [drain](drain.md) | Gracefully stop accepting new jobs |
| [jobs](jobs.md) | List and manage running jobs |
| [config](config.md) | Validate the configuration file |
| [images](images.md) | List cached container images |
| [doctor](doctor.md) | Check system readiness and clean up stale state |
| [ctrctl](ctrctl.md) | Debug the embedded containerd (passthrough to ctr) |

## Global flags

- `--data-dir <path>` — data directory for ephemerd state (default: `/var/lib/ephemerd` on Linux, `C:\ProgramData\ephemerd` on Windows)
- `--help` / `-h` — show help
- `--version` / `-v` — print version
