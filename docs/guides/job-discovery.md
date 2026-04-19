---
title: Job Discovery
weight: 2
---

ephemerd discovers queued CI jobs through three modes: polling, webhook via tunnel, and webhook via direct TLS. Each mode has different tradeoffs around latency, infrastructure requirements, and API quota usage.

## Polling (Default)

Polling is the simplest mode. ephemerd periodically calls the GitHub API to check for queued workflow jobs. No inbound network connectivity is required -- the host only makes outbound HTTPS requests.

```mermaid
sequenceDiagram
    participant GH as GitHub API
    participant E as ephemerd

    loop Every 10s (default)
        E->>GH: GET /repos/.../actions/runs?status=queued
        GH-->>E: List of queued jobs
        E->>E: Filter by labels, dedup
    end

    Note over E: Job found
    E->>GH: POST /repos/.../actions/runners/generate-jitconfig
    GH-->>E: JIT runner config
    E->>E: Create container, start runner
```

**Pros:** Zero configuration beyond a token. Works behind NAT, firewalls, and corporate proxies.

**Cons:** Jobs are picked up on the next poll cycle, adding up to one poll interval of latency. Each poll consumes one API request per repository.

### Rate Limit Math

GitHub rate limits depend on authentication method:

| Auth method | Requests/hour | At 10s interval | At 30s interval |
|-------------|--------------|-----------------|-----------------|
| Personal Access Token | 5,000 | ~360/hr/repo | ~120/hr/repo |
| GitHub App | 15,000 | ~360/hr/repo | ~120/hr/repo |

For a PAT monitoring 10 repositories at the default 10-second interval, that is roughly 3,600 requests per hour -- well within the 5,000 limit. A GitHub App gets 15,000 requests per hour, making it the better choice for organizations with many repositories.

### Configuration

Polling is the default mode. The only required configuration is authentication and the target organization:

```toml
[github]
owner = "your-org"
# repos = ["repo1", "repo2"]   # optional: limit to specific repos
# poll_interval = "10s"         # default: 10s
```

Set the token via environment variable:

```bash
export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

### GitHub App Setup

For higher rate limits and finer-grained permissions, use a GitHub App instead of a PAT:

1. Create a GitHub App at `https://github.com/organizations/YOUR-ORG/settings/apps/new`
2. Set the following permissions:
   - **Repository permissions:** Actions (read), Administration (read/write)
   - **Organization permissions:** Self-hosted runners (read/write)
3. Generate a private key (PEM file) and download it
4. Install the app on your organization
5. Note the App ID and Installation ID

```toml
[github]
owner = "your-org"
app_id = 123456
installation_id = 789012
private_key_path = "/var/lib/ephemerd/app.pem"
```

## Webhook via Tunnel

For near-instant job pickup, ephemerd can receive webhook events from GitHub. When running behind NAT (the common case for homelab and on-prem setups), ephemerd creates a public tunnel and automatically registers a webhook with GitHub.

```mermaid
sequenceDiagram
    participant GH as GitHub
    participant T as Tunnel (ngrok/localtunnel)
    participant E as ephemerd

    E->>T: Create tunnel
    T-->>E: Public URL
    E->>GH: Register webhook (public URL)
    GH-->>E: Webhook ID

    Note over GH,E: Webhook active

    GH->>T: POST /webhook (workflow_job event)
    T->>E: Forward request
    E->>E: Verify HMAC signature
    E->>E: Create container, start runner

    Note over E: On shutdown
    E->>GH: Delete webhook
```

The webhook URL changes on every restart (random subdomain), but this does not matter -- ephemerd registers a fresh webhook each time and cleans up the old one on shutdown. The webhook secret is auto-generated if not explicitly set.

If the tunnel connection drops, ephemerd automatically reconnects and re-registers the webhook. After a configurable number of consecutive failures (default 5), it falls back to polling mode so jobs are not missed.

### localtunnel

localtunnel is free, requires no account, and can be self-hosted. The public localtunnel.me server works for testing. For production use, deploy a localtunnel server on a cheap VPS — a $5/month Linode is more than enough. See [examples/localtunnel](https://github.com/ephpm/ephemerd/tree/main/examples/localtunnel) for a Terraform config that sets one up.

```toml
[webhook]
tunnel = "localtunnel"
# tunnel_url = "https://tunnels.example.com"   # self-hosted server
```

### ngrok

ngrok is more reliable than localtunnel and offers custom domains on paid plans. The free tier allows one endpoint and 20,000 requests per month.

```toml
[webhook]
tunnel = "ngrok"
ngrok_authtoken = "your-ngrok-token"
# Or set NGROK_AUTHTOKEN environment variable
```

## Webhook via Direct TLS

For hosts with a public IP address and a TLS certificate, ephemerd can listen for webhooks directly without a tunnel. This requires manual webhook setup in GitHub.

### Configuration

```toml
[webhook]
secret = "your-webhook-secret"
port = 8080
tls_cert = "/etc/ephemerd/tls.crt"
tls_key = "/etc/ephemerd/tls.key"
```

### GitHub Webhook Setup

1. Go to your repository (or organization) settings, then **Webhooks** > **Add webhook**
2. Set **Payload URL** to `https://your-host:8080/webhook`
3. Set **Content type** to `application/json`
4. Set **Secret** to the same value as `webhook.secret` in your config
5. Under **Which events would you like to trigger this webhook?**, select **Let me select individual events** and check only **Workflow jobs**
6. Save the webhook

Unlike tunnel mode, direct TLS does not auto-register or auto-deregister webhooks. You manage the webhook configuration in GitHub manually.
