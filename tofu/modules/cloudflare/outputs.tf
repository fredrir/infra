output "record_ids" {
  description = "DNS record ids by hostname/type, for audits against the zone listing."
  value = {
    a     = { for h, r in cloudflare_dns_record.a : h => r.id }
    cname = { for h, r in cloudflare_dns_record.tunnel_cname : h => r.id }
  }
}
