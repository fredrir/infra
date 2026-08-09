output "id" {
  description = "Server ID."
  value       = hcloud_server.this.id
}

output "ipv4_address" {
  description = "Public IPv4."
  value       = hcloud_server.this.ipv4_address
}

output "firewall_id" {
  description = "Firewall ID."
  value       = hcloud_firewall.this.id
}
