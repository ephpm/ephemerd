---
title: Getting Started
weight: 1
---

## Install

Download the latest binary from [Releases](https://github.com/ephpm/ephemerd/releases), then:

```bash
sudo ./ephemerd install
```

This copies the binary to `/usr/local/bin/`, creates a default config at `/var/lib/ephemerd/config.toml`, and installs a systemd service (Linux), launchd plist (macOS), or Windows service.

Or build from source with `mage build`.

## Configure

```bash
sudo vim /var/lib/ephemerd/config.toml   # set github.owner
sudo vim /etc/default/ephemerd           # set GITHUB_TOKEN
```

## Start

```bash
sudo systemctl start ephemerd
sudo systemctl enable ephemerd   # start on boot
```

Or run manually:

```bash
export GITHUB_TOKEN="ghp_your_token_here"
sudo -E ephemerd serve
```

## Target it from your workflow

```yaml
runs-on: [self-hosted, linux, x64]
```

## Uninstall

```bash
sudo ephemerd uninstall
```

This stops the service, removes the binary, service files, and data directory. Use `--keep-data` to preserve your config and logs.
