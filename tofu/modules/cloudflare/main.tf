# llunde.no zone records (ADR 012). DNS-only (grey) for the hosts Caddy serves
# with Let's Encrypt; the phase-4 orange-cloud decision is a reviewed diff of
# `proxied` here — never a dashboard click. parser/external stay proxied CNAMEs
# to the pyparser tunnel until phase 3.5 touches that box's ingress.

locals {
  direct_hosts = toset(["llunde.no", "www.llunde.no", "api.llunde.no"])
  tunnel_hosts = toset(["parser.llunde.no", "external.llunde.no"])
}

resource "cloudflare_dns_record" "a" {
  for_each = local.direct_hosts

  zone_id = var.zone_id
  name    = each.value
  type    = "A"
  content = var.ipv4
  proxied = false
  ttl     = 1 # auto
}

resource "cloudflare_dns_record" "aaaa" {
  for_each = local.direct_hosts

  zone_id = var.zone_id
  name    = each.value
  type    = "AAAA"
  content = var.ipv6
  proxied = false
  ttl     = 1
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

# ---- Email (Amazon SES domain identity) ----
# Found in the zone during the phase-3 import audit; imported verbatim.

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
  content = "\"v=DMARC1; p=none; rua=mailto:fhansteen@gmail.com\""
  ttl     = 1
}
