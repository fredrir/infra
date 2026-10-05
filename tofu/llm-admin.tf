locals {
  llm_admin_host = "llm-admin.fredrir.com"
  access_github  = "ae4b9d76-c28a-44a9-9056-b6da60405ccb"
}

data "cloudflare_zero_trust_organization" "access" {
  account_id = local.cf_account
}

resource "cloudflare_zero_trust_access_policy" "llm_admin" {
  account_id = local.cf_account
  name       = "llm-admin"
  decision   = "allow"
  include    = [{ email = { email = "fhansteen@gmail.com" } }]
}

resource "cloudflare_zero_trust_access_application" "llm_admin" {
  account_id                = local.cf_account
  name                      = "llm-admin"
  domain                    = local.llm_admin_host
  type                      = "self_hosted"
  session_duration          = "24h"
  allowed_idps              = [local.access_github]
  auto_redirect_to_identity = true
  app_launcher_visible      = false
  policies = [{
    id         = cloudflare_zero_trust_access_policy.llm_admin.id
    precedence = 1
  }]
}
