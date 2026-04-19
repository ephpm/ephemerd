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

**Linux (systemd):**

```bash
sudo systemctl start ephemerd
sudo systemctl enable ephemerd   # start on boot
```

**macOS (launchd):**

```bash
sudo launchctl load /Library/LaunchDaemons/dev.ephpm.ephemerd.plist
```

**Manual (any platform):**

```bash
export GITHUB_TOKEN=ghp_...
sudo -E ephemerd serve
```

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
