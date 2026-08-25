provider "cloudflare" {}

module "cloudflare" {
  source = "./modules/cloudflare"

  zone_id            = "4ae54b24fc4140d4d1c450491645f1c8"
  account_id         = local.cf_account
  pyparser_tunnel_id = "e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9"
  llunde_tunnel_id   = local.llunde_tunnel
}
