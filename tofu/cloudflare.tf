# llunde.no zone under tofu (ADR 012). Token comes from the CLOUDFLARE_API_TOKEN
# env var (Doppler llunde/ops), never git or state. Scope: `Zone:DNS:Edit` on
# llunde.no PLUS `Account:Cloudflare Tunnel:Read` and `:Edit` — the read half is
# not optional, since every plan REFRESHES the tunnel config resource.
#
# ⚠️ Laptop-only, owner-held, and NEVER to be copied to a host: Cloudflare cannot
# scope a Cloudflare-Tunnel permission to a single tunnel, so this token reaches
# all three in the account, including the portfolio and pyparser tenants' across
# the ADR 016 boundary. Caddy's DNS-01 token is SEPARATE and `Zone:DNS:Edit`-only,
# in sops.
provider "cloudflare" {}

module "cloudflare" {
  source = "./modules/cloudflare"

  zone_id            = "4ae54b24fc4140d4d1c450491645f1c8"
  account_id         = local.cf_account
  pyparser_tunnel_id = "e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9"
  llunde_tunnel_id   = local.llunde_tunnel
}
