output "server_id" {
  description = "llunde server ID."
  value       = module.server.id
}

output "server_ipv4" {
  description = "llunde server public IPv4."
  value       = module.server.ipv4_address
}
