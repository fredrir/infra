variable "name" {
  description = "hcloud_server name — must match the live server exactly (ForceNew)."
  type        = string
}

variable "server_type" {
  description = "Hetzner server type, e.g. ccx23 (ForceNew — must match live)."
  type        = string
}

variable "location" {
  description = "Hetzner location, e.g. hel1 (ForceNew — must match live)."
  type        = string
}

variable "image" {
  description = "Image used at create time only; ignored via lifecycle on adopted servers."
  type        = string
  default     = "ubuntu-24.04"
}

variable "ssh_keys" {
  description = "SSH key names/ids set at create only; ignored via lifecycle. Empty for adopted servers."
  type        = list(string)
  default     = []
}

variable "firewall_name" {
  description = "Name of the network firewall placed in front of the server."
  type        = string
}

variable "inbound_rules" {
  description = "Inbound TCP allow rules. e.g. [{port=\"22\",description=\"SSH\"}]. Each renders source_ips 0.0.0.0/0 + ::/0."
  type = list(object({
    port        = string
    description = string
  }))
}

variable "delete_protection" {
  description = "Hetzner delete protection."
  type        = bool
  default     = true
}

variable "rebuild_protection" {
  description = "Hetzner rebuild protection."
  type        = bool
  default     = true
}
