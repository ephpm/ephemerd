---
title: "^# "
---


## Problem

ephemerd runs on homelab machines, dev laptops, and small VPS instances — places without stable public IPs or ingress. GitHub and GitLab need to POST webhook events to a public URL. Polling works but adds latency (up to 10s) and burns API quota. Webhooks are instant and free.

## Solution: TunnelProvider Interface

ephemerd embeds tunnel clients as Go libraries. When webhook mode is enabled and a tunnel provider is configured, ephemerd creates a public URL automatically — no manual ngrok/cloudflared setup, no port forwarding, no reverse proxy config.

### Interface

```go
// TunnelProvider creates a publicly-reachable listener for receiving webhooks.
type TunnelProvider interface {
    // Listen creates a tunnel and returns a net.Listener with a public URL.
    // The listener is closed when ctx is cancelled.
    Listen(ctx context.Context) (net.Listener, error)

    // PublicURL returns the public URL of the tunnel after Listen succeeds.
    PublicURL() string
}
```

Both ngrok-go and localtunnel return `net.Listener`, so the scheduler just swaps its `server.Serve(listener)` call — no protocol adapters, no synthetic wrappers.

### Scheduler Integration

In `Scheduler.Run()`, when a tunnel provider is configured:

1. Create the tunnel listener via `s.cfg.Tunnel.Listen(ctx)`
2. Register webhooks with GitHub via `s.cfg.GitHub.RegisterWebhooks(ctx, url, secret)`
3. Serve HTTP on the tunnel listener
4. On shutdown, `DeregisterWebhooks()` fires via `defer` — removes all webhooks from GitHub

```go
if s.cfg.Tunnel != nil && useWebhook {
    ln, err := s.cfg.Tunnel.Listen(ctx)
    // ...
    webhookURL := s.cfg.Tunnel.PublicURL() + "/webhook"
    hooks, err := s.cfg.GitHub.RegisterWebhooks(ctx, webhookURL, s.cfg.WebhookSecret)
    defer s.cfg.GitHub.DeregisterWebhooks(context.Background(), hooks)
    go server.Serve(ln)
}
```

The webhook handler, signature verification, and event channel all stay the same. Only the listener source changes. Webhook registration and deregistration are fully automatic — no manual GitHub settings needed.

### Automatic Webhook Lifecycle

`pkg/github/webhook.go` manages the webhook lifecycle:

- **`RegisterWebhooks(ctx, url, secret)`** — creates `workflow_job` webhooks on each configured repo (or org-level). On partial failure, cleans up any hooks already created before returning an error.
- **`DeregisterWebhooks(ctx, hooks)`** — removes all managed webhooks. Called on shutdown via `defer`.

This means the tunnel URL can change on every restart (random subdomain) and it doesn't matter — ephemerd registers a fresh webhook each time and cleans up the old one.

## Providers

### ngrok-go

**Import:** `golang.ngrok.com/ngrok`

**Auth:** requires `NGROK_AUTHTOKEN` (free tier: 1 endpoint, 20K requests/month).

**Implementation:**

```go
type NgrokTunnel struct {
    listener net.Listener
    url      string
}

func (n *NgrokTunnel) Listen(ctx context.Context) (net.Listener, error) {
    ln, err := ngrok.Listen(ctx,
        config.HTTPEndpoint(),
        ngrok.WithAuthtokenFromEnv(),
    )
    if err != nil {
        return nil, fmt.Errorf("ngrok listen: %w", err)
    }
    n.listener = ln
    n.url = ln.URL()
    return ln, nil
}

func (n *NgrokTunnel) PublicURL() string {
    return n.url
}
```

**Pros:** rock-solid, well-maintained, custom domains on paid plans, built-in TLS.
**Cons:** requires auth token, free tier has request limits.

### localtunnel

**Import:** `github.com/ephpm/ephemerd/pkg/localtunnel` (vendored from `github.com/localtunnel/go-localtunnel` with context support added)

**Auth:** none. Fully free, no account needed.

**Implementation:**

```go
type LocalTunnel struct {
    listener net.Listener
    url      string
}

func (lt *LocalTunnel) Listen(ctx context.Context) (net.Listener, error) {
    ln, err := localtunnel.Listen(localtunnel.Options{})
    if err != nil {
        return nil, fmt.Errorf("localtunnel listen: %w", err)
    }
    lt.listener = ln
    lt.url = "https://" + ln.Addr().String()

    // Close listener when context is cancelled
    go func() {
        <-ctx.Done()
        ln.Close()
    }()

    return ln, nil
}

func (lt *LocalTunnel) PublicURL() string {
    return lt.url
}
```

**Pros:** zero auth, zero config, fully free, self-hostable server.
**Cons:** less reliable than ngrok, no custom domains, community-maintained.

### Self-hosted localtunnel server

localtunnel's server (`localtunnel-server`) can be self-hosted. This is the best option for production homelab setups that want zero dependency on third-party SaaS. Deploy it on a cheap VPS and point ephemerd at it:

```go
ln, err := localtunnel.Listen(localtunnel.Options{
    BaseURL: "https://tunnels.example.com",
})
```

## Config

```toml
[webhook]
secret = "your-webhook-secret"

# Tunnel provider: "ngrok", "localtunnel", or "" (no tunnel, listen on port directly)
tunnel = "ngrok"

# localtunnel: optional self-hosted server URL
# tunnel_url = "https://tunnels.example.com"

# ngrok: auth token (can also use NGROK_AUTHTOKEN env var)
# ngrok_authtoken = "your-token"
```

When `tunnel` is empty or omitted, ephemerd listens on the webhook port directly (current behavior). This is the right choice when running behind a reverse proxy or on a VPS with a public IP.

## Self-Hosted localtunnel on Linode

For production homelab use, self-hosting the localtunnel server eliminates all third-party dependencies. A Linode Nanode ($5/month) is more than enough.

See `examples/localtunnel/` for a complete Terraform configuration that deploys a localtunnel server on Linode (Miami datacenter) with userdata-driven setup.

```toml
[webhook]
secret = "your-github-webhook-secret"
tunnel = "localtunnel"
tunnel_url = "http://tunnels.example.com"
```

ephemerd registers the webhook with GitHub automatically on startup and removes it on shutdown. No manual GitHub configuration needed.

---

## Footnotes

### Why Not Cloudflare Tunnel

Cloudflare Quick Tunnels (trycloudflare.com) look attractive on paper — free, no auth, Cloudflare's edge network. In practice they are unusable as an embedded Go library.

The problem is architectural. ngrok-go and localtunnel give you a `net.Listener` — your code accepts connections normally. Cloudflare's tunnel protocol is inverted: their edge opens QUIC streams toward your process, decodes them through a Cap'n Proto RPC layer, and delivers them via an internal `OriginProxy.ProxyHTTP(ResponseWriter, *Request)` handler interface. There is no listener. There is no socket. Traffic arrives as function calls deep inside cloudflared's supervisor stack.

To embed this you would need to import ~10 tightly coupled internal packages from `github.com/cloudflare/cloudflared` (supervisor, orchestrator, connection, edgediscovery, tunnelrpc, proxy, etc.), bootstrap the full tunnel daemon lifecycle, inject a custom `OriginProxy` implementation to intercept HTTP requests, and build a synthetic `net.Listener` that wraps handler calls back into `net.Conn` objects.

The dependency footprint is massive: Cap'n Proto code generation, `quic-go`, Sentry, OpenTelemetry, and more. The internal APIs are unstable — these are private packages of a CLI tool, not a library. There is no Go SDK and Cloudflare has declined to build one despite community requests (cloudflare/cloudflared#986).

Quick tunnels also have no SLA, no uptime guarantee, single-connection only (hardcoded `ha-connections=1`), random subdomains with no option for custom domains, and Cloudflare explicitly reserves the right to terminate them at any time.

If you want Cloudflare Tunnel, run `cloudflared tunnel` as a separate process and point it at ephemerd's webhook port. That works fine — it's just not embeddable.
