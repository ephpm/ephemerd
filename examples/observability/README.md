# Observability rig

A self-contained Prometheus + Grafana setup that scrapes ephemerd's
`/metrics` endpoint and renders a reference dashboard with daemon-level
and per-container resource panels.

## Quick start

1. Enable the metrics endpoint in your ephemerd config
   (`<data-dir>/config.toml`):

   ```toml
   [metrics]
   enabled = true
   port = 9090
   path = "/metrics"
   # container_stats_interval = "10s"   # default
   ```

   Restart ephemerd. Confirm with `curl http://localhost:9090/metrics`.

2. Start the rig:

   ```sh
   cd examples/observability
   docker compose up -d
   ```

3. Open Grafana at <http://localhost:3000>. Anonymous Admin is enabled
   for the rig, so no login prompt. The "ephemerd" dashboard is
   pre-provisioned.

   Prometheus is reachable at <http://localhost:9091> if you need to
   inspect raw series or experiment in the expression browser.

## What gets scraped

The compose config points Prometheus at `host.docker.internal:9090` —
the host where ephemerd is running.

- On **Docker Desktop** (Windows/macOS): `host.docker.internal`
  resolves automatically. No config needed.
- On **Linux Docker**: `host.docker.internal` is mapped to
  `host-gateway` via `extra_hosts`. Works out of the box.

If your ephemerd lives on a different machine, override `EPHEMERD_TARGET`
in `.env` (copy `.env.example`).

## Adding more nodes

`EPHEMERD_TARGET` is a comma-separated `host:port` list. To scrape a fleet
of ephemerd nodes on the same local network, list them all:

```
EPHEMERD_TARGET=windows-pc.lan:9090,macmini.lan:9090,linux-box.lan:9090
```

Then `docker compose restart prom-config prometheus` (the init container
re-renders `prometheus.yml`).

Prometheus tags every series with `instance="<host:port>"` automatically,
so the dashboard panels split per node without further config — just add
`{instance="..."}` to any query, or add a Grafana template variable on
`label_values(instance)` to get a filter dropdown.

**Reachability tips:**

- Bind ephemerd's metrics server to all interfaces (it already does, via
  `port = 9090` in `[metrics]`). Open port 9090 in the host firewall on
  each node.
- Use `.lan` / mDNS hostnames or static IPs — whatever resolves from the
  box running the Compose rig. If the rig runs on Podman/WSL on Windows,
  remember the resolver lives inside the Podman machine VM, not on the
  Windows host.
- LAN only — no TLS / auth wired here. Don't expose `/metrics` to the
  internet without setting `metrics.tls_cert` / `tls_key` in `config.toml`
  on the ephemerd side and switching Prom to `https://...`.

## Cardinality + retention

Per-container series (`ephemerd_container_*`) carry an `id` label that
is unique per job. ephemerd deletes the series on container destroy,
so live cardinality is bounded by `max_concurrent`. Prometheus still
retains historical samples for the configured TSDB retention (default
14 days; tune via the `--storage.tsdb.retention.time` flag in the
compose file).

## Useful queries

```promql
# CPU rate per container, percent of cap (>=1 means hitting the cap):
rate(ephemerd_container_cpu_usage_seconds_total[1m])
  / on(id, repo, runtime) ephemerd_container_cpu_limit

# Memory headroom remaining:
ephemerd_container_memory_limit_bytes - ephemerd_container_memory_bytes

# Active jobs by repo:
sum by (repo) (
  count by (id, repo) (ephemerd_container_memory_bytes)
)

# p95 job duration by repo:
histogram_quantile(0.95,
  sum by (repo, le) (rate(ephemerd_job_duration_seconds_bucket[5m])))

# Per-job total egress over the last hour (job-by-job):
sum by (id, repo) (
  increase(ephemerd_container_network_tx_bytes_total[1h])
)

# Aggregate download rate per repo:
sum by (repo) (rate(ephemerd_container_network_rx_bytes_total[5m]))
```

Network counters cover the **runner container's netns only**. Sibling
containers spawned via the fake Docker socket get their own netns and
are not represented; see the architecture doc for the full caveat.
