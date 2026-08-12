output "record_ids" {
  description = "DNS record ids by hostname/type, for audits against the zone listing."
  value = {
    llunde = { for h, r in cloudflare_dns_record.llunde_tunnel_cname : h => r.id }
    cname  = { for h, r in cloudflare_dns_record.tunnel_cname : h => r.id }
  }
}
