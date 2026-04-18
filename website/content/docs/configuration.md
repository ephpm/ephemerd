---
title: Configuration
weight: 3
---

ephemerd uses a single TOML config file at `<data-dir>/config.toml`.

```toml
[github]
# Authentication: PAT or GITHUB_TOKEN env var
# token = "ghp_..."                  # or set GITHUB_TOKEN env var
owner = "your-org"                    # org or user
# repos = ["repo1", "repo2"]         # optional — omit for org-level runners

[webhook]
# Default: none (polling). Set to "localtunnel" or "ngrok" for instant delivery.
# tunnel = "localtunnel"             # zero-config tunnel (recommended)
# tunnel_url = "http://tunnels.example.com"  # self-hosted localtunnel server
# tunnel = "ngrok"                   # use ngrok instead (requires auth token)
# ngrok_authtoken = "..."            # or set NGROK_AUTHTOKEN env var

[runner]
max_concurrent = 4                    # parallel jobs
extra_labels = []                     # additional runner labels
job_timeout = "2h"                    # kill jobs after this
shutdown_timeout = "5m"               # wait for running jobs on SIGTERM

# Cross-OS Linux VM (Windows and macOS hosts only)
[vm.linux]
enabled = true                        # boot a Linux VM for Linux jobs
cpus = 2
memory_mb = 2048
disk_size_gb = 50                     # sparse — only uses space as needed

# macOS-native jobs (macOS hosts only)
# No enable/disable toggle — macOS VMs always run on darwin hosts.
[vm.macos]
disk_image = "/path/to/macos.img"    # base disk image (or auto-pulled from Tart OCI registry)
cpus = 4
memory_mb = 8192
max_concurrent = 2                    # max simultaneous macOS VMs (default: auto-detected)

[network]
# subnet = "10.88.0.0/16"           # container network subnet
# mtu = 1500

[dind]
# enabled = false                    # mount fake Docker socket into containers

[metrics]
# enabled = false                    # Prometheus metrics endpoint
# port = 9090
# path = "/metrics"

[log]
level = "info"                        # debug, info, warn, error
format = "text"                       # text or json
log_retention = "7d"                  # max age for job log files
```
