provider "aws" {
  region = "eu-north-1"
}

provider "cloudflare" {}

locals {
  cloudflare_account = "8786559b30fcebd08d0c594b6e899eef"
  cloudflare_groups  = { for group in data.cloudflare_account_api_token_permission_groups_list.account.result : group.name => group.id }
  reconciler_source  = ["${hcloud_primary_ip.reconciler.ip_address}/32"]
}

resource "aws_iam_access_key" "verify" {
  user = "infra-reconciliation-verify"
}

data "cloudflare_account_api_token_permission_groups_list" "account" {
  account_id = local.cloudflare_account
}

resource "cloudflare_account_token" "verify" {
  account_id = local.cloudflare_account
  name       = "infra-reconciliation-verify"

  policies = [
    {
      effect            = "allow"
      permission_groups = [for name in ["Zone Read", "DNS Read"] : { id = local.cloudflare_groups[name] }]
      resources         = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = { "com.cloudflare.api.account.zone.*" = "*" } })
    },
    {
      effect            = "allow"
      permission_groups = [{ id = local.cloudflare_groups["Cloudflare Tunnel Read"] }]
      resources         = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = "*" })
    },
  ]

  condition = {
    request_ip = {
      in = local.reconciler_source
    }
  }
}
