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

If your ephemerd lives on a different machine, edit `prometheus.yml`
and change the target to that machine's address.

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
