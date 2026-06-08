global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'ephemerd'
    metrics_path: /metrics
    static_configs:
      - targets:
          # Rendered at boot by the prom-config service from the
          # EPHEMERD_TARGET env var (comma-separated host:port list).
          # Default is host.docker.internal:9090 — works on Docker Desktop.
          # See README "Adding more nodes" + .env.example.
__EPHEMERD_TARGETS__
