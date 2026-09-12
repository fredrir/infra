locals {
  platform_tunnels = {
    grafana = {
      id   = "0aef0efb-868d-4dba-862b-b4c3a72a83fa"
      name = "platform-grafana"
      ingress = [
        {
          service  = "http://monitoring-grafana.observability.svc.cluster.local:80"
          hostname = "grafana.fredrir.com"
        },
        {
          service = "http_status:404"
        },
      ]
    }
    cache = {
      id   = "899938ad-d232-4906-8d81-f77b3b32f6c9"
      name = "platform-attic"
      ingress = [
        {
          service  = "http://attic.nix-cache.svc.cluster.local:8080"
          hostname = "cache.fredrir.com"
        },
        {
          service = "http_status:404"
        },
      ]
    }
    y = {
      id   = "1c45c3ed-a9be-4011-916f-d859f5ec1d88"
      name = "platform-y"
      ingress = [
        {
          service  = "http://web.y.svc.cluster.local:8080"
          hostname = "yeeter.no"
        },
        {
          service  = "http://web.y.svc.cluster.local:8080"
          hostname = "www.yeeter.no"
        },
        {
          service = "http_status:404"
        },
      ]
    }
    ingress = {
      id   = "39622de5-5e59-4821-9bdc-b9276c837275"
      name = "platform-ingress"
      ingress = [
        {
          hostname = "*.fredrir.com"
          service  = "http://traefik.ingress-system.svc.cluster.local:80"
        },
        {
          hostname = "fredrir.com"
          service  = "http://traefik.ingress-system.svc.cluster.local:80"
        },
        {
          service = "http_status:404"
        },
      ]
    }
  }
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "platform" {
  for_each = local.platform_tunnels

  account_id = local.cf_account
  name       = each.value.name
  config_src = "cloudflare"

  lifecycle {
    prevent_destroy = true
  }
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "platform" {
  for_each = local.platform_tunnels

  account_id = local.cf_account
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.platform[each.key].id
  config = {
    ingress = each.value.ingress
  }
}

import {
  for_each = local.platform_tunnels

  to = cloudflare_zero_trust_tunnel_cloudflared.platform[each.key]
  id = "${local.cf_account}/${each.value.id}"
}

import {
  for_each = local.platform_tunnels

  to = cloudflare_zero_trust_tunnel_cloudflared_config.platform[each.key]
  id = "${local.cf_account}/${each.value.id}"
}
