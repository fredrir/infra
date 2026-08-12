# llunde.no zone records (ADR 012). DNS-only (grey) for the hosts Caddy serves
# with Let's Encrypt; the phase-4 orange-cloud decision is a reviewed diff of
# `proxied` here — never a dashboard click. parser/external stay proxied CNAMEs
# to the pyparser tunnel until phase 3.5 touches that box's ingress.

locals {
  direct_hosts = toset(["llunde.no", "www.llunde.no", "api.llunde.no"])
  tunnel_hosts = toset(["parser.llunde.no", "external.llunde.no"])
}

# E5a (phase-4 cutover, step 1 of 2). The AAAA records for these three names are
# GONE, deliberately and on their own, before E5b turns the A records into
# proxied CNAMEs on the llunde tunnel.
#
# A CNAME cannot coexist with an A *or* an AAAA record at the same name
# (Cloudflare error 81053), and tofu gives NO ordering guarantee between an
# unrelated create and destroy in one apply — so doing both at once can fail,
# and can fail non-deterministically. Splitting it makes each apply a single
# reviewable fact.
#
# IPv6 is not lost, only relocated: after E5b these names resolve through
# Cloudflare's anycast edge, which is dual-stack. The v6 gap lasts between the
# two applies. See runbook §7.1.
resource "cloudflare_dns_record" "a" {
  for_each = local.direct_hosts

  zone_id = var.zone_id
  name    = each.value
  type    = "A"
  content = var.ipv4
  proxied = false
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

# ---- llunde tunnel ingress map (phase-4 B2, ADR 017) ----
# The hostname -> origin routing the connector fetches at startup. It was
# dashboard-created in E1 and is adopted here so the front door's routing is a
# reviewed diff like everything else, and so the stale `edge-test` hostname from
# the E3 real-IP proof is removed declaratively rather than by remembering to
# click it.
#
# All three vhosts point at ONE Caddy listener, which routes by Host — that is
# why the map is this boring. `localhost` (not 127.0.0.1) is verbatim what the
# live config has carried since E1 and is proven in production: the connector
# runs Network=host, so this is the host's loopback, and Caddy binds
# 127.0.0.1:8085. Kept byte-identical on purpose so adopting the config is a
# no-op apart from dropping edge-test.
#
# The catch-all is required and must be last: cloudflared refuses a config whose
# final rule has a hostname. `service` is required on every rule.
locals {
  llunde_tunnel_origin = "http://localhost:8085"
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
  content = "\"v=DMARC1; p=quarantine; rua=mailto:fhansteen@gmail.com\""
  ttl     = 1
}
