variable "reconciler_hcloud_token" {
  type      = string
  sensitive = true
}

variable "bootstrap_admin_cidrs" {
  type    = set(string)
  default = []

  validation {
    condition = alltrue([
      for cidr in var.bootstrap_admin_cidrs :
      can(regex("^[0-9]{1,3}(\\.[0-9]{1,3}){3}/[0-9]{1,2}$", cidr)) && can(cidrhost(cidr, 0)) && try(tonumber(split("/", cidr)[1]) >= 24, false)
    ])
    error_message = "Bootstrap SSH is limited to explicit administrator IPv4 ranges of /24 or narrower."
  }
}
