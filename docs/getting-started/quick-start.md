---
title: Quick Start
weight: 2
---

This guide walks through getting ephemerd running and picking up its first job. It assumes you have already [installed ephemerd]({{< relref "installation" >}}).

## 1. Configure

Edit the config file created during installation:

- **Linux / macOS:** `/var/lib/ephemerd/config.toml`
- **Windows:** `C:\ProgramData\ephemerd\config.toml`

At minimum, set your GitHub organization or user:

```toml
[github]
owner = "your-org"
```

Set a GitHub token with `admin:org` scope (or `repo` scope for repo-level runners) as an environment variable:

```bash
export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

For systemd, write the token to an environment file so the service can read it:

```bash
echo 'GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' | sudo tee /etc/default/ephemerd
```

See the [Configuration]({{< relref "configuration" >}}) page for the full reference, including GitHub App authentication and alternative providers (Forgejo, Gitea, GitLab, Woodpecker).

## 2. Start the service

ephemerd includes cross-platform service management commands so you don't need to remember `systemctl` vs `launchctl` vs `sc.exe`:

```bash
ephemerd install     # copy binary, create config, register system service
ephemerd start       # start the service
ephemerd stop        # stop the service
ephemerd restart     # restart the service
ephemerd uninstall   # remove binary, service, and data
```

These work the same on Linux, macOS, and Windows. To start ephemerd and have it run on boot:

```bash
sudo ephemerd start
```

To run manually in the foreground (useful for debugging):

```bash
export GITHUB_TOKEN=ghp_...
sudo -E ephemerd serve
```

### macOS: first boot takes time

On macOS, the first time ephemerd starts it downloads a Tart base disk image from the OCI registry. This is a multi-gigabyte download and can take a while depending on your connection. Subsequent starts use the cached image and boot in seconds. Plan for this on the first run — don't expect macOS VM jobs to work immediately after install.

## 3. Target from a workflow

In your GitHub Actions workflow, use the `self-hosted` label along with the platform label:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
    steps:
      - uses: actions/checkout@v4
      - run: echo "Running on ephemerd"
```

Push this workflow and ephemerd will pick up the job, create an isolated container, run it, and destroy the environment when it finishes.

## 4. Verify

Check that the daemon is healthy and processing jobs:

```bash
ephemerd status
```

This shows the daemon status, active job count, concurrency limit, and uptime.

To see currently running jobs:

```bash
ephemerd jobs
```

To validate your configuration without starting the daemon:

```bash
ephemerd config check
```
