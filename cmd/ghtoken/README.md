# ghtoken

Mints a GitHub App installation token from ephemerd's config and prints it to stdout.

## Build

```
go build -o ghtoken ./cmd/ghtoken
```

## Usage

```bash
# Uses /var/lib/ephemerd/config.toml by default
export GH_TOKEN=$(./ghtoken)

# Or point at a different config
export EPHEMERD_CONFIG=/path/to/config.toml
export GH_TOKEN=$(./ghtoken)

# Then use with gh CLI
gh workflow run test-arm64-smoke.yml
gh api repos/ephpm/ephemerd/actions/runs --jq '.workflow_runs[:3] | .[].status'
```

## Requirements

The config file must have GitHub App auth configured:

```toml
[github]
app_id = 123456
installation_id = 789012
private_key_path = "~/.ssh/your-app.pem"
```

PAT-based auth (`github.token`) won't work — this tool specifically mints short-lived installation tokens from the App private key.
