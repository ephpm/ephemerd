---
title: config
weight: 11
---

Validate the configuration file without starting the daemon. Prints a summary of the parsed configuration and reports any errors.

```
ephemerd config [flags]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config`, `-c` | `<data-dir>/config.toml` | Path to config file |

## Output

On success, prints a summary of the parsed configuration:

```
Config: /var/lib/ephemerd/config.toml
  GitHub owner:    myorg
  Repos:           [repo1 repo2]
  Max concurrent:  4
  Job timeout:     6h0m0s
  Poll interval:   30s
  Log level:       info
  Mode:            webhook (tunnel: localtunnel)
  Auth:            token (set)

Config OK
```

The summary includes:

- GitHub owner and repository list
- Runner concurrency and timeout settings
- Poll interval
- Log level
- Job discovery mode (polling, webhook with tunnel, or webhook with TLS)
- Authentication method (token or GitHub App)

On failure, prints the parse or validation error and exits with a non-zero status.

## Examples

```bash
# Validate the default config
ephemerd config

# Validate a specific config file
ephemerd config --config /etc/ephemerd/config.toml
ephemerd config -c ./config.toml
```
