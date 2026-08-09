variable "zone_id" {
  description = "Cloudflare zone id for llunde.no."
  type        = string
}

variable "ipv4" {
  description = "Public IPv4 the direct (grey-cloud) records point at — llunde-01."
  type        = string
}

variable "ipv6" {
  description = "Public IPv6 address for the direct records — llunde-01."
  type        = string
}

variable "pyparser_tunnel_id" {
  description = <<-EOT
    Tunnel id behind parser/external CNAMEs. The tunnel itself is deliberately
    NOT managed here — its token embeds a secret that a tofu apply would rotate,
    dropping the live connectors (docs/cloudflare.md). Records only.
  EOT
  type        = string
}
