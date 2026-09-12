variable "platform_dns_zones" {
  type    = map(string)
  default = {}

  validation {
    condition = alltrue([
      for name, id in var.platform_dns_zones :
      can(regex("^[a-z0-9][a-z0-9.-]*\\.[a-z]{2,}$", name)) && can(regex("^[a-f0-9]{32}$", id)) && name != "llunde.no"
    ])
    error_message = "Use verified existing zone IDs keyed by domain; llunde.no remains owned by the legacy module."
  }
}

variable "platform_dns_records" {
  type = map(object({
    zone      = string
    name      = string
    type      = string
    content   = string
    ttl       = optional(number, 1)
    proxied   = optional(bool, false)
    priority  = optional(number)
    import_id = optional(string)
  }))
  default = {}

  validation {
    condition = alltrue([
      for key, record in var.platform_dns_records :
      can(regex("^[a-z][a-z0-9_-]*$", key)) && contains(keys(var.platform_dns_zones), record.zone) &&
      (record.import_id == null ? true : can(regex("^[a-f0-9]{32}$", record.import_id)))
    ])
    error_message = "Records require stable keys, a configured zone and an optional verified Cloudflare record ID for import."
  }
}

module "platform_dns" {
  source = "./modules/platform-dns"

  zones   = var.platform_dns_zones
  records = var.platform_dns_records
}

import {
  for_each = { for key, record in var.platform_dns_records : key => record if record.import_id != null }

  to = module.platform_dns.cloudflare_dns_record.records[each.key]
  id = "${var.platform_dns_zones[each.value.zone]}/${each.value.import_id}"
}

output "platform_dns_records" {
  value = module.platform_dns.records
}
