# llunde.no zone under tofu (ADR 012, phase-3 step 2). Token comes from the
# CLOUDFLARE_API_TOKEN env var (Doppler llunde/ops) — scoped to Zone:DNS on
# llunde.no only, never in git or state.
provider "cloudflare" {}

module "cloudflare" {
  source = "./modules/cloudflare"

  zone_id            = "4ae54b24fc4140d4d1c450491645f1c8"
  ipv4               = hcloud_server.llunde_01.ipv4_address
  ipv6               = hcloud_server.llunde_01.ipv6_address
  pyparser_tunnel_id = "e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9"
}
