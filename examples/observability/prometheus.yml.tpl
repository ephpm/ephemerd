global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'ephemerd'
    metrics_path: /metrics
    static_configs:
      - targets:
          # Rendered at boot by the prom-config service from the
          # EPHEMERD_TARGET env var. Default is host.docker.internal:9090
          # which works on Docker Desktop. Podman / rootless / Linux users
          # may need to override — see README "Engine-specific overrides".
          - '__EPHEMERD_TARGET__'
        labels:
          host: 'ephemerd'
