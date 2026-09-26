provider "aws" {
  region = "eu-north-1"
}

provider "cloudflare" {}

locals {
  cloudflare_account = "8786559b30fcebd08d0c594b6e899eef"
  cloudflare_groups  = { for group in data.cloudflare_account_api_token_permission_groups_list.account.result : group.name => group.id... }
  reconciler_source  = ["${hcloud_primary_ip.reconciler.ip_address}/32"]
  managed_zones = {
    "fredrir.com" = "6a5d7959f34aa2fb76d2d5c7509b32ff"
    "llunde.no"   = "4ae54b24fc4140d4d1c450491645f1c8"
    "yeeter.no"   = "1bfde4d22f052143ea46926366e50fca"
  }
}

resource "aws_iam_access_key" "verify" {
  user = "infra-reconciliation-verify"
}

resource "aws_iam_access_key" "apply" {
  user = "infra-reconciliation-apply"
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
      permission_groups = [for name in ["Zone Read", "DNS Read"] : { id = one(local.cloudflare_groups[name]) }]
      resources         = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = { "com.cloudflare.api.account.zone.*" = "*" } })
    },
    {
      effect            = "allow"
      permission_groups = [{ id = one(local.cloudflare_groups["Cloudflare Tunnel Read"]) }]
      resources         = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = "*" })
    },
  ]

  condition = {
    request_ip = {
      in = local.reconciler_source
    }
  }
}

resource "cloudflare_account_token" "apply" {
  account_id = local.cloudflare_account
  name       = "infra-reconciliation-apply"

  policies = [
    {
      effect            = "allow"
      permission_groups = [for name in ["Zone Read", "DNS Write"] : { id = one(local.cloudflare_groups[name]) }]
      resources = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = {
        for zone in values(local.managed_zones) : "com.cloudflare.api.account.zone.${zone}" => "*"
      } })
    },
    {
      effect            = "allow"
      permission_groups = [{ id = one(local.cloudflare_groups["Cloudflare Tunnel Write"]) }]
      resources         = jsonencode({ "com.cloudflare.api.account.${local.cloudflare_account}" = "*" })
    },
  ]

  condition = {
    request_ip = {
      in = local.reconciler_source
    }
  }
}
