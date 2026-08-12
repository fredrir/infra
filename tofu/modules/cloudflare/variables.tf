variable "zone_id" {
  description = "Cloudflare zone id for llunde.no."
  type        = string
}

variable "account_id" {
  description = <<-EOT
    Cloudflare account id. Needed only for the llunde tunnel's INGRESS map
    (below) — tunnel resources are account-scoped, not zone-scoped.

    Consequence, verified 2026-08-12 and deliberately accepted: Cloudflare
    cannot scope a Cloudflare-Tunnel permission to a single tunnel. The ops
    token that plans this root therefore reaches all three tunnels in the
    account — llunde, hansteen-portfolio-origin and pyparser-review — across the
    ADR 016 tenant boundary. It is a laptop-only, owner-held credential and must
    never be copied to a host.
  EOT
  type        = string
}

variable "llunde_tunnel_id" {
  description = <<-EOT
    The llunde tunnel (ADR 017) that fronts llunde.no/www/api. Unlike
    pyparser_tunnel_id this one's CONFIG is managed here, not just its records —
    but still never its token, which embeds a secret an apply would rotate,
    dropping the live connectors.
  EOT
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
