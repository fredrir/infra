output "records" {
  value = {
    for key, record in cloudflare_dns_record.records : key => {
      id      = record.id
      zone_id = record.zone_id
      name    = record.name
      type    = record.type
    }
  }
}
