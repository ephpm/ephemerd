---
title: Providers
weight: 4
---

ephemerd supports multiple Git forges via a provider interface. Configure one provider per instance.

## GitHub (default)

```toml
[github]
owner = "your-org"
# repos = ["repo1", "repo2"]         # optional — omit for org-level runners
# Authentication: PAT via GITHUB_TOKEN env var, or GitHub App:
# app_id = 123456
# installation_id = 789012
# private_key_path = "/path/to/app.pem"
```

## Forgejo

```toml
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"   # from Forgejo admin > Actions > Runners
owner = "your-org"
# repos = ["repo1", "repo2"]         # optional — omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"  # default job execution image
```

## Gitea

```toml
[gitea]
instance_url = "https://gitea.example.com"
token = "runner-registration-token"   # from Gitea admin > Actions > Runners
owner = "your-org"
# repos = ["repo1", "repo2"]         # optional — omit for all repos
# job_image = "gitea/runner-images:ubuntu-24.04"  # default job execution image
```

## GitLab

```toml
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"           # runner auth token (GitLab 16+)
tags = ["linux", "docker", "ephemerd"]
```

## Woodpecker CI

```toml
[woodpecker]
server_url = "woodpecker.example.com:9000"   # Woodpecker server gRPC URL
agent_secret = "your-shared-secret"          # agent authentication secret
```

## Auto-detection

The provider is auto-detected from which config section has credentials set. Precedence: Forgejo > Gitea > GitLab > Woodpecker > GitHub (default).
