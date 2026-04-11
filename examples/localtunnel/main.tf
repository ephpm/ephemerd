terraform {
  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 2.0"
    }
  }
}

provider "linode" {
  token = var.linode_token
}

variable "linode_token" {
  description = "Linode API token (create at https://cloud.linode.com/profile/tokens)"
  sensitive   = true
}

variable "tunnel_domain" {
  description = "Domain for the tunnel server (e.g. tunnels.example.com). Requires a wildcard DNS record."
}

variable "region" {
  description = "Linode region"
  default     = "us-mia"
}

variable "authorized_keys" {
  description = "SSH public keys for root access"
  type        = list(string)
}

resource "linode_instance" "localtunnel" {
  label  = "localtunnel-server"
  region = var.region
  type   = "g6-nanode-1" # 1 GB RAM, 1 CPU, $5/month
  image  = "linode/ubuntu24.04"

  authorized_keys = var.authorized_keys

  metadata {
    user_data = base64encode(templatefile("${path.module}/userdata.sh", {
      tunnel_domain = var.tunnel_domain
    }))
  }
}

output "ip_address" {
  value       = linode_instance.localtunnel.ip_address
  description = "Public IP — create DNS A records pointing here"
}

output "tunnel_url" {
  value       = "http://${var.tunnel_domain}"
  description = "Base URL for ephemerd webhook.tunnel_url config"
}

output "dns_records" {
  value       = <<-EOT
    Create these DNS records:
      A    ${var.tunnel_domain}      → ${linode_instance.localtunnel.ip_address}
      A    *.${var.tunnel_domain}    → ${linode_instance.localtunnel.ip_address}
  EOT
  description = "Required DNS records (wildcard is needed for tunnel subdomains)"
}
