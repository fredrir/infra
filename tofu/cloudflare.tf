# llunde.no zone under tofu (ADR 012, phase-3 step 2). Token comes from the
# CLOUDFLARE_API_TOKEN env var (Doppler llunde/ops), never in git or state.
#
# Scope, widened in phase-4 B2 so the llunde tunnel's ingress map could leave the
# dashboard: `Zone:DNS:Edit` on llunde.no PLUS `Account:Cloudflare Tunnel:Read`
# and `:Edit`. The read half is not optional — a plan REFRESHES the tunnel config
# resource, so every run needs it, not just runs that change something.
#
# ⚠️ Laptop-only, owner-held. Cloudflare cannot scope a Cloudflare-Tunnel
# permission to a single tunnel (verified 2026-08-12), so this token reaches all
# three tunnels in the account — including the portfolio and pyparser tenants',
# across the ADR 016 boundary. It must never be copied to a host. The DNS-01
# token Caddy uses is a SEPARATE, `Zone:DNS:Edit`-only token in sops.
provider "cloudflare" {}

module "cloudflare" {
  source = "./modules/cloudflare"

  zone_id            = "4ae54b24fc4140d4d1c450491645f1c8"
  account_id         = local.cf_account
  ipv4               = hcloud_server.llunde_01.ipv4_address
  ipv6               = hcloud_server.llunde_01.ipv6_address
  pyparser_tunnel_id = "e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9"
  llunde_tunnel_id   = local.llunde_tunnel
}
