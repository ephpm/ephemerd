# CLI Reference

ephemerd is a single binary with several subcommands.

| Command | Description |
|---|---|
| [serve](serve.md) | Start the daemon — the main command |
| [run](run.md) | Run a workflow locally without pushing to GitHub |
| [status](status.md) | Show running jobs, health, uptime |
| [drain](drain.md) | Gracefully stop accepting new jobs |
| [jobs](jobs.md) | List and manage running jobs |
| [config](config.md) | Validate the configuration file |
| [doctor](doctor.md) | Check system readiness and clean up stale state |
| [install](install.md) | Install binary and register as a system service |
| [uninstall](uninstall.md) | Remove ephemerd from the system |

## Global flags

- `--data-dir <path>` — data directory for ephemerd state (default: `/var/lib/ephemerd` on Linux, `C:\ProgramData\ephemerd` on Windows)
- `--help` / `-h` — show help
- `--version` / `-v` — print version
