---
title: Webhooks and Tunnels
weight: 5
---

ephemerd supports two job discovery modes: polling the forge API at an interval, or receiving instant webhook events via an auto-provisioned tunnel. Tunnels work behind NAT with zero inbound port requirements.

## Problem

ephemerd runs on homelab machines, dev laptops, and small VPS instances -- places without stable public IPs or ingress. Forges need to POST webhook events to a public URL. Polling works but adds latency (up to the poll interval) and consumes API quota. Webhooks are instant and free.

## TunnelProvider Interface

ephemerd embeds tunnel clients as Go libraries. When webhook mode is enabled and a tunnel provider is configured, ephemerd creates a public URL automatically -- no manual ngrok/cloudflared setup, no port forwarding, no reverse proxy config.

The interface in `pkg/tunnel/tunnel.go`:

```go
type Provider interface {
    // Listen creates a tunnel and returns a net.Listener with a public URL.
    // The tunnel is torn down when the listener is closed or ctx is cancelled.
    Listen(ctx context.Context) (net.Listener, error)

    // PublicURL returns the public URL of the tunnel after Listen succeeds.
    PublicURL() string
}
```

Both ngrok-go and localtunnel return `net.Listener`, so the scheduler just swaps its `server.Serve(listener)` call -- no protocol adapters or synthetic wrappers needed.

## Scheduler Integration

In `Scheduler.Run()`, when a tunnel provider is configured:

1. Create the tunnel listener via `tunnel.Listen(ctx)`.
2. Register webhooks with the forge via `RegisterWebhooks(ctx, url, secret)`.
3. Serve HTTP on the tunnel listener.
4. On shutdown, `DeregisterWebhooks()` fires via `defer` -- removes all webhooks from the forge.

The webhook handler, signature verification, and event channel all stay the same regardless of transport. Only the listener source changes. Webhook registration and deregistration are fully automatic -- no manual forge settings needed.

### Automatic Webhook Lifecycle

`pkg/github/webhook.go` manages the webhook lifecycle:

- **`RegisterWebhooks(ctx, url, secret)`** -- creates `workflow_job` webhooks on each configured repo (or org-level). On partial failure, cleans up any hooks already created before returning an error.
- **`DeregisterWebhooks(ctx, hooks)`** -- removes all managed webhooks. Called on shutdown via `defer`.

The tunnel URL can change on every restart (random subdomain) and it does not matter -- ephemerd registers a fresh webhook each time and cleans up the old one.

## Providers

### ngrok-go

**Package:** `golang.ngrok.com/ngrok`

**Auth:** requires `NGROK_AUTHTOKEN` (free tier: 1 endpoint, 20K requests/month).

Pros: reliable, well-maintained, custom domains on paid plans, built-in TLS.
Cons: requires auth token, free tier has request limits.

### localtunnel

**Package:** `pkg/tunnel/localtunnel.go` (vendored from `github.com/localtunnel/go-localtunnel` with context support added).

**Auth:** none. Fully free, no account needed.

Pros: zero auth, zero config, fully free, self-hostable server.
Cons: less reliable than ngrok, no custom domains, community-maintained.

### Self-hosted localtunnel

localtunnel's server can be self-hosted on a cheap VPS. This is the best option for production homelab setups that want zero dependency on third-party SaaS. See `examples/localtunnel/` for a complete Terraform configuration that deploys a localtunnel server on Linode.

## Configuration

```toml
[webhook]
secret = "your-webhook-secret"

# Tunnel provider: "none" (default, polling), "localtunnel", or "ngrok"
tunnel = "ngrok"

# localtunnel: optional self-hosted server URL
# tunnel_url = "https://tunnels.example.com"

# ngrok: auth token (can also use NGROK_AUTHTOKEN env var)
# ngrok_authtoken = "your-token"

# max consecutive reconnect failures before falling back to polling
# tunnel_max_retries = 5
```

When `tunnel` is `"none"` or omitted, ephemerd polls the forge API at the configured interval. This is the right default when running behind a reverse proxy or on a VPS with a public IP.

## Why Not Cloudflare Tunnel

Cloudflare Quick Tunnels look attractive on paper -- free, no auth, Cloudflare's edge network. In practice they are not embeddable as a Go library.

The problem is architectural. ngrok-go and localtunnel give you a `net.Listener` -- your code accepts connections normally. Cloudflare's tunnel protocol is inverted: their edge opens QUIC streams toward your process and delivers traffic via an internal `OriginProxy.ProxyHTTP` handler interface. There is no listener. There is no socket.

To embed this would require importing ~10 tightly coupled internal packages from `cloudflared`, bootstrapping the full tunnel daemon lifecycle, and building a synthetic `net.Listener` wrapper. The dependency footprint includes Cap'n Proto code generation, quic-go, Sentry, and OpenTelemetry. The internal APIs are unstable -- these are private packages of a CLI tool, not a library.

If you want Cloudflare Tunnel, run `cloudflared tunnel` as a separate process and point it at ephemerd's webhook port. That works fine -- it is just not embeddable.
