variable "zones" {
  type = map(string)
}

variable "records" {
  type = map(object({
    zone     = string
    name     = string
    type     = string
    content  = string
    ttl      = optional(number, 1)
    proxied  = optional(bool, false)
    priority = optional(number)
  }))

  validation {
    condition = alltrue([
      for record in values(var.records) :
      contains(keys(var.zones), record.zone) &&
      (record.name == "@" || record.name == record.zone || endswith(record.name, ".${record.zone}")) &&
      contains(["A", "AAAA", "CNAME", "TXT", "MX", "NS"], record.type) && length(record.content) > 0 &&
      (record.ttl == 1 || (record.ttl >= 60 && record.ttl <= 86400 && floor(record.ttl) == record.ttl)) &&
      (!record.proxied || (contains(["A", "AAAA", "CNAME"], record.type) && record.ttl == 1)) &&
      (record.type == "MX" ? try(record.priority >= 0 && record.priority <= 65535 && floor(record.priority) == record.priority, false) : record.priority == null)
    ])
    error_message = "Records must belong to their zone, use supported types and valid TTLs, and specify priority only for MX."
  }
}
