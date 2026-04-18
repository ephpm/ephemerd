---
title: "^# "
---


Validates the configuration file without starting the daemon. Useful for checking config syntax after editing.

## Usage

```
ephemerd config [--data-dir <path>] [--config <path>]
```

## What it does

1. Loads and parses `<data-dir>/config.toml`
2. Validates required fields (github.owner, authentication)
3. Checks default values are applied correctly
4. Prints the resolved configuration (with secrets redacted)
5. Exits with code 0 if valid, 1 if invalid

## Example output

```
Config file: /var/lib/ephemerd/config.toml

[github]
  owner = "myorg"
  token = (set)
  repos = [repo1, repo2]
  poll_interval = "30s"

[webhook]
  tunnel = "none"
  port = 8080

[runner]
  max_concurrent = 4
  job_timeout = "2h"
  shutdown_timeout = "5m"
```
