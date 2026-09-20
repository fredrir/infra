variable "retained_grafana_hosts" {
  type    = set(string)
  default = []

  validation {
    condition     = alltrue([for host in var.retained_grafana_hosts : can(regex("^[a-z0-9][a-z0-9.-]*[.]fredrir[.]com$", host))])
    error_message = "Grafana routes must be hosts in fredrir.com."
  }
}

locals {
  grafana_host  = yamldecode(file("${path.module}/../platform/clusters/production/settings.yaml")).data.GRAFANA_HOST
  grafana_hosts = setunion(toset([local.grafana_host]), var.retained_grafana_hosts)
}

resource "cloudflare_dns_record" "grafana" {
  for_each = local.grafana_hosts

  zone_id = var.platform_dns_zones["fredrir.com"]
  name    = each.key
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.platform["grafana"].id}.cfargotunnel.com"
  ttl     = 1
  proxied = true

  lifecycle {
    create_before_destroy = true
    precondition {
      condition     = can(regex("^[a-z0-9][a-z0-9.-]*[.]fredrir[.]com$", local.grafana_host))
      error_message = "Grafana must use a hostname in fredrir.com."
    }
  }
}

moved {
  from = module.platform_dns.cloudflare_dns_record.records["grafana"]
  to   = cloudflare_dns_record.grafana["grafana.fredrir.com"]
}

output "grafana_hosts" {
  value = local.grafana_hosts
}
