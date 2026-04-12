# ephemerd serve

Start the ephemerd daemon. This is the main command — it starts containerd, connects to GitHub, and begins provisioning runners for queued jobs.

## Usage

```
ephemerd serve [--data-dir <path>]
```

## What it does

1. Loads config from `<data-dir>/config.toml`
2. Starts the embedded containerd server (in-process, no external binary)
3. Extracts embedded runner, CNI, and shim binaries into the data directory
4. Sets up container networking (CNI bridge on Linux, HCN on Windows)
5. Installs firewall rules blocking containers from private networks
6. Starts the health endpoint on the configured webhook port (default 8080)
7. Connects to GitHub via polling or webhook tunnel
8. For each queued job: creates an isolated container, starts the runner, destroys on completion
9. On SIGTERM: drains running jobs (waits up to `shutdown_timeout`), deregisters webhooks, cleans up

## Environment variables

- `GITHUB_TOKEN` — GitHub personal access token (if not set in config)

## Flags

- `--data-dir` — data directory for ephemerd state (default: `/var/lib/ephemerd` on Linux, `C:\ProgramData\ephemerd` on Windows)

## Ports

- Webhook/health port (default 8080) — serves `/healthz` and `/webhook`
- Metrics port (default 9090, disabled unless `[metrics] enabled = true`) — serves `/metrics`

## Signals

- `SIGTERM` / `SIGINT` — graceful shutdown (drain running jobs, then exit)
