# Self-Hosted localtunnel Server on Linode

Deploys a localtunnel server on a Linode Nanode ($5/month, Miami datacenter) for ephemerd webhook delivery. No third-party tunnel services, no ngrok account needed.

## How It Works

1. Terraform creates a Nanode running `localtunnel-server` on port 80
2. You point a domain (with wildcard) at the Nanode's IP
3. ephemerd connects to your server, gets a unique subdomain (e.g. `abc123.tunnels.example.com`)
4. GitHub POSTs webhook events to that subdomain
5. localtunnel-server forwards them over a TCP tunnel to your local ephemerd

## TLS

localtunnel-server runs plain HTTP. The webhook payload contains only job IDs and metadata — no secrets. The `webhook.secret` HMAC signature guarantees integrity regardless of transport.

If you want HTTPS anyway, put the Nanode behind a Linode NodeBalancer with a TLS cert, or proxy DNS through Cloudflare with the orange cloud enabled (free, automatic TLS at Cloudflare's edge).

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/install)
- [Linode API token](https://cloud.linode.com/profile/tokens) (Linodes read/write)
- A domain you control

## Deploy

```bash
cd examples/localtunnel

cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars with your values

terraform init
terraform apply
```

## DNS

After `terraform apply`, create two A records pointing to the output IP:

```
A    tunnels.example.com      → <ip_address>
A    *.tunnels.example.com    → <ip_address>
```

The wildcard is required — each tunnel client gets a unique subdomain.

## ephemerd Config

```toml
[webhook]
secret = "your-github-webhook-secret"
tunnel = "localtunnel"
tunnel_url = "http://tunnels.example.com"
```

When ephemerd starts, it logs the public webhook URL:

```
INFO webhook tunnel ready url=http://abc123.tunnels.example.com/webhook
```

## GitHub Webhook Setup

In your GitHub repo or org settings, add a webhook:

- **Payload URL:** the URL ephemerd logged above
- **Content type:** `application/json`
- **Secret:** same value as `webhook.secret` in your TOML
- **Events:** select "Workflow jobs"

## Teardown

```bash
terraform destroy
```
