---
title: serve
weight: 1
---

Start the ephemerd daemon. This is the default command -- running `ephemerd` without a subcommand is equivalent to `ephemerd serve`.

```
ephemerd serve [flags]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config`, `-c` | `<data-dir>/config.toml` | Path to config file |
| `--containerd-tcp-port` | (none) | Also expose containerd on a TCP port (used by the in-VM worker so the Windows host can reach containerd over TCP) |
| `--containerd-tcp-addr` | `127.0.0.1` | Bind address for the containerd TCP listener (use `0.0.0.0` when host lives outside the network namespace) |
| `--containerd-only` | `false` | Only run containerd and the dispatch worker (no scheduler, no GitHub polling, no runner extraction) |
| `--dind` | `false` | Mount a fake Docker socket into each container (overrides config file setting) |

## Startup sequence

When `serve` starts, it performs these steps in order:

1. **Check for existing instance** -- connects to the gRPC control socket to verify no other ephemerd is already running.
2. **Load configuration** -- reads the TOML config file from `--config` or `<data-dir>/config.toml`.
3. **Write PID file** -- writes `ephemerd.pid` to the data directory (used by the `drain` command).
4. **Start container runtime** -- launches the embedded containerd server in-process. On macOS, this boots a Linux VM via Virtualization.framework instead.
5. **Extract embedded runner** -- unpacks the GitHub Actions runner binary (`.tar.gz` on Linux, `.zip` on Windows).
6. **Extract CNI plugins** -- unpacks the embedded CNI plugin binaries.
7. **Initialize networking** -- sets up the container network (CNI bridge on Linux, HCN NAT on Windows).
8. **Install firewall rules** -- blocks container access to RFC1918 and link-local ranges.
9. **Create GitHub client** -- authenticates using `GITHUB_TOKEN` or GitHub App credentials from the config.
10. **Wait for Linux dispatcher** -- if a Hyper-V Linux VM is booting in the background (Windows only), waits for the gRPC dispatch client to become ready.
11. **Configure webhook tunnel** -- sets up localtunnel or ngrok for webhook delivery, or falls back to polling mode.
12. **Start scheduler** -- begins discovering and processing GitHub Actions jobs.
13. **Start metrics server** -- if metrics are enabled in the config, starts the Prometheus metrics endpoint.

## Containerd-only mode

When `--containerd-only` is set, the daemon runs a stripped-down mode intended for the in-VM Linux worker (Hyper-V Linux VM on Windows, Vz Linux VM on macOS):

- Starts containerd with the TCP listener.
- Extracts the runner and CNI plugins.
- Cleans stale CNI bridges from previous boots.
- Sets up networking and firewall rules.
- Starts the gRPC dispatch server on `containerd-tcp-port + 1`.
- Does **not** start the scheduler, poll GitHub, or require GitHub credentials.

The host dispatches Linux jobs to this worker via gRPC.

## Signal handling

The daemon listens for `SIGINT` and `SIGTERM`. On receipt, it stops accepting new jobs and waits for running jobs to finish before exiting. The PID file is cleaned up on exit.

## Environment variables

| Variable | Description |
|----------|-------------|
| `GITHUB_TOKEN` | GitHub personal access token (alternative to setting `github.token` in config) |
| `EPHEMERD_DATA_DIR` | Override the default data directory |

## Ports

- **Webhook / health endpoint**: configured via `webhook.port` in config (default varies by setup)
- **Metrics**: configured via `metrics.port` in config, only active when `metrics.enabled = true`

## Examples

```bash
# Start with default config
sudo ephemerd serve

# Start with a custom config file
sudo ephemerd serve --config /etc/ephemerd/config.toml

# Start in in-VM worker mode (invoked automatically inside the Hyper-V or Vz Linux VM)
ephemerd serve --containerd-tcp-port 10000 --containerd-tcp-addr 0.0.0.0 --containerd-only

# Start with Docker-in-Docker support
sudo ephemerd serve --dind
```
