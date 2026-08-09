output "ipv4" {
  description = "Public IPv4 of llunde-01"
  value       = hcloud_server.llunde_01.ipv4_address
}

output "ipv6" {
  description = "Public IPv6 network of llunde-01"
  value       = hcloud_server.llunde_01.ipv6_address
}
