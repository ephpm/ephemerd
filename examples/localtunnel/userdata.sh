#!/bin/bash
set -euo pipefail

# localtunnel server setup for ephemerd webhook delivery.
# localtunnel-server handles its own TLS via --secure flag when given
# the domain. Caddy is NOT used here — localtunnel-server needs direct
# access to ports 80/443 for both HTTP requests and TCP tunnel connections.

export DEBIAN_FRONTEND=noninteractive

# Install Node.js 20 LTS
curl -fsSL https://deb.nodesource.com/setup_20.x | bash -
apt-get install -y nodejs

# Install localtunnel server
npm install -g @localtunnel/server

# localtunnel-server listens on port 80 directly.
# It handles subdomain routing and tunnel TCP connections internally.
# TLS termination is left to an external load balancer or DNS proxy
# (e.g. Cloudflare DNS proxy with orange cloud, or Linode NodeBalancer).
#
# For plain HTTP (acceptable when GitHub webhook secret provides HMAC
# integrity, and the payload contains no secrets — just job IDs):
cat > /etc/systemd/system/localtunnel.service <<EOF
[Unit]
Description=localtunnel server
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/lt-server --port 80 --domain ${tunnel_domain}
Restart=always
RestartSec=5
Environment=NODE_ENV=production

[Install]
WantedBy=multi-user.target
EOF

# Open firewall
ufw allow 80/tcp 2>/dev/null || true

systemctl daemon-reload
systemctl enable --now localtunnel
