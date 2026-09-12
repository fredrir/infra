resource "cloudflare_dns_record" "records" {
  for_each = var.records

  zone_id  = var.zones[each.value.zone]
  name     = each.value.name == "@" ? each.value.zone : each.value.name
  type     = each.value.type
  content  = each.value.content
  ttl      = each.value.ttl
  proxied  = each.value.proxied
  priority = each.value.priority

  lifecycle {
    prevent_destroy = true
  }
}
