locals {
  llunde_hosts = toset(["llunde.no", "www.llunde.no", "api.llunde.no"])
  tunnel_hosts = toset(["parser.llunde.no", "external.llunde.no"])
}

moved {
  from = cloudflare_dns_record.a
  to   = cloudflare_dns_record.llunde_tunnel_cname
}

resource "cloudflare_dns_record" "llunde_tunnel_cname" {
  for_each = local.llunde_hosts

  zone_id = var.zone_id
  name    = each.value
  type    = "CNAME"
  content = "${var.llunde_tunnel_id}.cfargotunnel.com"
  proxied = true
  ttl     = 1 # auto
}

resource "cloudflare_dns_record" "tunnel_cname" {
  for_each = local.tunnel_hosts

  zone_id = var.zone_id
  name    = each.value
  type    = "CNAME"
  content = "${var.pyparser_tunnel_id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
  comment = lookup({
    "external.llunde.no" = "pyparser Convert public share origin"
  }, each.value, null)
}

locals {
  llunde_tunnel_origin = "http://caddy.llunde.svc.cluster.local:8080"
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "llunde" {
  account_id = var.account_id
  tunnel_id  = var.llunde_tunnel_id

  config = {
    ingress = [
      {
        hostname = "llunde.no"
        service  = local.llunde_tunnel_origin
      },
      {
        hostname = "www.llunde.no"
        service  = local.llunde_tunnel_origin
      },
      {
        hostname = "api.llunde.no"
        service  = local.llunde_tunnel_origin
      },
      {
        service = "http_status:404"
      },
    ]
  }
}

locals {
  ses_dkim_tokens = toset([
    "3a3cp6jxlzhfahnvx5rj6pjb3f55rn4f",
    "63zt7tsduuapiy4tttuh2pyidd237aoe",
    "qtpkbhn7eweqzxahnj5ydaeihstnrcel",
  ])
}

resource "cloudflare_dns_record" "ses_dkim" {
  for_each = local.ses_dkim_tokens

  zone_id = var.zone_id
  name    = "${each.value}._domainkey.llunde.no"
  type    = "CNAME"
  content = "${each.value}.dkim.amazonses.com"
  proxied = false
  ttl     = 1
}

resource "cloudflare_dns_record" "spf" {
  zone_id = var.zone_id
  name    = "llunde.no"
  type    = "TXT"
  content = "\"v=spf1 include:amazonses.com ~all\""
  ttl     = 1
}

resource "cloudflare_dns_record" "dmarc" {
  zone_id = var.zone_id
  name    = "_dmarc.llunde.no"
  type    = "TXT"
  content = "\"v=DMARC1; p=quarantine; rua=mailto:fhansteen@gmail.com\""
  ttl     = 1
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "pyparser" {
  account_id = var.account_id
  tunnel_id  = var.pyparser_tunnel_id

  config = {
    origin_request = {}
    ingress = [
      {
        hostname = "parser.llunde.no"
        path     = "^/logs(/.*)?$"
        service  = "http://traefik.ingress-system.svc.cluster.local:80"
      },
      {
        hostname = "parser.llunde.no"
        service  = "http://review:8081"
      },
      {
        hostname = "external.llunde.no"
        path     = "^/media/share/.*"
        service  = "http://review:8081"
      },
      {
        hostname = "external.llunde.no"
        service  = "http_status:404"
      },
      {
        service = "http_status:404"
      },
    ]
  }
}
