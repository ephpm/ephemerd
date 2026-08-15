---
title: Webhooks and Tunnels
weight: 5
---

ephemerd supports two job discovery modes: polling the forge API at an interval, or receiving instant webhook events via an auto-provisioned tunnel. Tunnels work behind NAT with zero inbound port requirements.

## Problem

ephemerd runs on homelab machines, dev laptops, and small VPS instances -- places without stable public IPs or ingress. Forges need to POST webhook events to a public URL. Polling works but adds latency (up to the poll interval) and consumes API quota. Webhooks are instant and free.

## TunnelProvider Interface

Every tunnel mode sits behind one interface. `tunnel = "localtunnel"` and `"ngrok"` are embedded Go libraries; `"cloudflared"` is a managed subprocess ephemerd downloads and owns (see [below](#why-cloudflare-tunnel-is-a-managed-subprocess-not-an-embedded-library)); `"external"` means the ingress is created and managed outside ephemerd entirely; `"none"` (the default) means polling. In every managed mode ephemerd creates the public URL and registers the forge webhook automatically -- no port forwarding and no reverse proxy config.

| `tunnel` | Ingress | Managed by ephemerd |
|---|---|---|
| `none` (default) | none -- polls the forge API instead | n/a |
| `localtunnel` | localtunnel (public or self-hosted server) | yes, embedded library |
| `ngrok` | ngrok edge | yes, embedded library |
| `cloudflared` | Cloudflare Tunnel on a hostname you own | yes, downloaded subprocess |
| `external` | whatever you put in front of the webhook port | no -- receiver only |

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

Both ngrok-go and localtunnel return `net.Listener` directly, so the scheduler just swaps its `server.Serve(listener)` call -- no protocol adapters or synthetic wrappers needed. The cloudflared provider satisfies the same interface by returning a plain local listener that its subprocess forwards to.

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

# Tunnel provider: "none" (default, polling), "localtunnel", "ngrok",
# "cloudflared", or "external"
tunnel = "ngrok"

# localtunnel: optional self-hosted server URL
# tunnel_url = "https://tunnels.example.com"

# ngrok: auth token (can also use NGROK_AUTHTOKEN env var)
# ngrok_authtoken = "your-token"

# cloudflared: tunnel token + public hostname (token can also use
# CLOUDFLARE_TUNNEL_TOKEN env var)
# cloudflared_token = "eyJhIjoi..."
# cloudflared_hostname = "ci.example.com"
# cloudflared_version = "2026.6.1"   # optional; pinned download version

# external: ingress managed outside ephemerd. Requires webhook.secret;
# set external_url to have ephemerd auto-register the hooks.
# external_url = "https://ci.example.com"

# max consecutive reconnect failures before falling back to polling
# tunnel_max_retries = 5
```

When `tunnel` is `"none"` or omitted, ephemerd polls the forge API at the configured interval. This is the right default when running behind a reverse proxy or on a VPS with a public IP.

## Why Cloudflare Tunnel Is a Managed Subprocess, Not an Embedded Library

Cloudflare Tunnel **is** supported (`tunnel = "cloudflared"`), but unlike ngrok and localtunnel it does not run as an in-process Go library. It runs as a subprocess that ephemerd downloads, launches, and owns.

The reason is architectural. ngrok-go and localtunnel give you a `net.Listener` -- ephemerd accepts connections normally and swaps the listener into `server.Serve()`. Cloudflare's tunnel protocol is inverted: their edge opens QUIC streams toward your process and delivers traffic via an internal `OriginProxy.ProxyHTTP` handler interface. There is no listener and no socket to hand back.

Embedding it would mean importing ~10 tightly coupled internal packages from `cloudflared`, bootstrapping the full tunnel daemon lifecycle, and building a synthetic `net.Listener` wrapper. The dependency footprint includes Cap'n Proto code generation, quic-go, Sentry, and OpenTelemetry, and the internal APIs are unstable -- these are private packages of a CLI tool, not a library.

So `pkg/tunnel/cloudflared.go` takes the other path: it downloads a pinned `cloudflared` release into `<data-dir>/cloudflared/`, writes its config, and runs it as a child process forwarding the public hostname to ephemerd's local webhook port. `Listen()` returns an ordinary local listener, so the scheduler integration above is unchanged. The subprocess is bound to ephemerd's lifetime -- on Linux via `Pdeathsig`, so it cannot outlive ephemerd even on SIGKILL or panic; best-effort via `Close()` elsewhere.

The trade-offs versus the embedded providers: it needs a Cloudflare zone and a tunnel token (created in the Cloudflare dashboard or API), and it depends on a downloaded external binary rather than compiled-in Go code. In exchange you get a stable custom hostname on Cloudflare's edge instead of an ephemeral random subdomain.

If you would rather run and manage `cloudflared` yourself -- say on a different host -- use `tunnel = "external"` instead. ephemerd then serves the webhook receiver and disables polling but never creates a tunnel.
