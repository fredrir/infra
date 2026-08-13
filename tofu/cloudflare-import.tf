# Declarative imports of the pre-existing llunde.no records. Ids are
# <zone_id>/<record_id> from the zone listing; kept after the apply as the audit
# trail, same as the server import block.

locals {
  cf_zone       = "4ae54b24fc4140d4d1c450491645f1c8"
  cf_account    = "8786559b30fcebd08d0c594b6e899eef"
  llunde_tunnel = "c0cdd9b5-fa97-42a1-bca7-95da236ea949"
}

# Adopts the llunde tunnel's ingress map, originally dashboard-created. Id form
# here is <account_id>/<tunnel_id> — no record id, the config is a singleton per
# tunnel. The HCL matches the live config, so anything appearing in a plan means
# the live config drifted and must be read (runbook §13.2) before applying.
import {
  to = module.cloudflare.cloudflare_zero_trust_tunnel_cloudflared_config.llunde
  id = "${local.cf_account}/${local.llunde_tunnel}"
}

import {
  to = module.cloudflare.cloudflare_dns_record.llunde_tunnel_cname["llunde.no"]
  id = "${local.cf_zone}/e257b8b6cfdc0b530f5b9179a519f82d"
}

import {
  to = module.cloudflare.cloudflare_dns_record.llunde_tunnel_cname["www.llunde.no"]
  id = "${local.cf_zone}/9417d77e16d3f3262abdaa7ee4ec051e"
}

import {
  to = module.cloudflare.cloudflare_dns_record.llunde_tunnel_cname["api.llunde.no"]
  id = "${local.cf_zone}/56c731500066e7e3d03baa85775eefbf"
}

import {
  to = module.cloudflare.cloudflare_dns_record.tunnel_cname["parser.llunde.no"]
  id = "${local.cf_zone}/0d50fbb4e9e2f35cbca26553d142e868"
}

import {
  to = module.cloudflare.cloudflare_dns_record.tunnel_cname["external.llunde.no"]
  id = "${local.cf_zone}/3468bf8dbf2fd8def438fc8001da1035"
}

import {
  to = module.cloudflare.cloudflare_dns_record.ses_dkim["3a3cp6jxlzhfahnvx5rj6pjb3f55rn4f"]
  id = "${local.cf_zone}/2c78ce505d031b79479e021d1c64938a"
}

import {
  to = module.cloudflare.cloudflare_dns_record.ses_dkim["63zt7tsduuapiy4tttuh2pyidd237aoe"]
  id = "${local.cf_zone}/1bad8d77dcf271ff4070ddf23bd35b83"
}

import {
  to = module.cloudflare.cloudflare_dns_record.ses_dkim["qtpkbhn7eweqzxahnj5ydaeihstnrcel"]
  id = "${local.cf_zone}/1d99f35ced15e59203020378a5ef311d"
}

import {
  to = module.cloudflare.cloudflare_dns_record.spf
  id = "${local.cf_zone}/14c50bdcde0f4e7e1e1aa95f52d1aa6e"
}

import {
  to = module.cloudflare.cloudflare_dns_record.dmarc
  id = "${local.cf_zone}/86077ea93a4f19c812c3109de2f3f50c"
}
