# llunde.no zone records (ADR 012). Every name is a proxied CNAME onto a tunnel:
# llunde.no/www/api onto the llunde tunnel, parser/external onto pyparser's. No
# record points at a host address, so the origin IP is not public — ADR 017
# replaced ADR 012's orange-cloud proxying of direct A records.

locals {
  llunde_hosts = toset(["llunde.no", "www.llunde.no", "api.llunde.no"])
  tunnel_hosts = toset(["parser.llunde.no", "external.llunde.no"])
}

# The AAAA records went in an apply of their own, before the A records became
# CNAMEs: a CNAME cannot coexist with an A *or* an AAAA at the same name (error
# 81053), and tofu gives NO ordering guarantee between an unrelated create and
# destroy in one apply, so doing both can fail non-deterministically. IPv6 is
# only relocated — these names resolve through Cloudflare's dual-stack anycast
# edge, pointing at the tunnel, not llunde-01 (runbook §7.1).
#
# ⚠️ A -> CNAME is a REPLACEMENT, not an in-place update: changing `type` forces
# replacement, so tofu destroys and re-creates with a seconds-long NXDOMAIN
# window no configuration can remove — create-before-destroy would collide with
# the record it replaces. The `moved` block still earns its place, carrying the
# STATE across so the replacement happens at ONE address rather than a create
# into a name still holding an A record (81053) plus a separate destroy.
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
  # MUST be proxied — a cfargotunnel.com target only resolves through the edge.
  # Grey-clouding is break-glass: put the A records back first (runbook §7.5).
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

# ---- llunde tunnel ingress map (ADR 017) ----
# The hostname -> origin routing the connector fetches at startup, managed here
# so the front door's routing is a reviewed diff. All three vhosts point at ONE
# Caddy listener that routes by Host, which is why the map is this boring.
# `localhost` (not 127.0.0.1) is verbatim what the live config carries and is
# proven in production: the connector runs Network=host, so this is the host's
# loopback, and Caddy binds 127.0.0.1:8085. The catch-all rule is required, must
# be LAST and must carry NO hostname — cloudflared refuses a config whose final
# rule has one. `service` is required on every rule.
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
# Pre-existing zone records, imported verbatim.

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
