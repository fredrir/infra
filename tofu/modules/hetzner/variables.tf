variable "name" {
  type        = string
}

variable "server_type" {
  type        = string
}

variable "location" {
  type        = string
}

variable "image" {
  type        = string
  default     = "ubuntu-24.04"
}

variable "ssh_keys" {
  type        = list(string)
  default     = []
}

variable "firewall_name" {
  type        = string
}

variable "inbound_rules" {
  type = list(object({
    port        = string
    description = string
  }))
}

variable "delete_protection" {
  type        = bool
  default     = true
}

variable "rebuild_protection" {
  type        = bool
  default     = true
}
