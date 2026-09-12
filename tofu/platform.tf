variable "platform_control_planes" {
  type = map(object({
    name       = string
    private_ip = string
    ssh_keys   = list(string)
  }))
  default = {}

  validation {
    condition = length(var.platform_control_planes) == 0 || (
      length(var.platform_control_planes) == 2 && alltrue([
        for node in values(var.platform_control_planes) :
        length(node.ssh_keys) > 0 && can(cidrhost("${node.private_ip}/32", 0)) && can(regex("^[a-z0-9][a-z0-9-]+$", node.name))
      ])
    )
    error_message = "Supply two named CX33 control planes with private IPv4 addresses and SSH key IDs, or leave disabled."
  }
}

variable "platform_network_cidr" {
  type    = string
  default = null
  validation {
    condition     = var.platform_network_cidr == null ? true : can(cidrhost(var.platform_network_cidr, 1))
    error_message = "A reviewed, non-overlapping network CIDR is required."
  }
}

variable "platform_control_plane_import_ids" {
  type    = map(string)
  default = {}

  validation {
    condition = alltrue([
      for key, id in var.platform_control_plane_import_ids :
      contains(keys(var.platform_control_planes), key) && can(regex("^[1-9][0-9]*$", id))
    ])
    error_message = "Existing servers require verified Hetzner IDs keyed by their configured stable control-plane keys."
  }
}

import {
  for_each = var.platform_control_plane_import_ids

  to = hcloud_server.platform_control_plane[each.key]
  id = each.value
}

variable "platform_subnet_cidr" {
  type    = string
  default = null
  validation {
    condition     = var.platform_subnet_cidr == null ? true : can(cidrhost(var.platform_subnet_cidr, 1))
    error_message = "A reviewed, non-overlapping subnet CIDR is required."
  }
}

variable "platform_admin_cidrs" {
  type    = set(string)
  default = []
  validation {
    condition     = alltrue([for cidr in var.platform_admin_cidrs : can(cidrhost(cidr, 0)) && !contains(["0.0.0.0/0", "::/0"], cidr)])
    error_message = "Use explicit administrative source CIDRs."
  }
}

variable "platform_existing_control_plane_networks" {
  type = map(object({
    server_id  = number
    private_ip = string
  }))
  default = {}

  validation {
    condition = (length(var.platform_existing_control_plane_networks) == 0 || length(var.platform_control_planes) == 2) && alltrue([
      for key, node in var.platform_existing_control_plane_networks :
      can(regex("^[a-z0-9][a-z0-9-]+$", key)) && node.server_id > 0 && floor(node.server_id) == node.server_id &&
      can(cidrnetmask("${node.private_ip}/32")) && !contains(values(var.platform_control_plane_import_ids), tostring(node.server_id)) &&
      !contains([for server in values(var.platform_control_planes) : server.private_ip], node.private_ip)
    ])
    error_message = "Existing control-plane attachments require distinct verified server IDs and private IPv4 addresses."
  }
}

variable "platform_existing_control_plane_spread_enabled" {
  type    = bool
  default = false

  validation {
    condition     = !var.platform_existing_control_plane_spread_enabled || length(var.platform_control_planes) == 2
    error_message = "The platform placement group must exist before assigning the existing control plane."
  }
}

locals {
  platform_enabled = length(var.platform_control_planes) > 0
}

resource "hcloud_network" "platform" {
  count = local.platform_enabled ? 1 : 0

  name     = "platform-production"
  ip_range = var.platform_network_cidr
  labels   = { managed-by = "opentofu", scope = "platform" }

  lifecycle {
    prevent_destroy = true
    precondition {
      condition     = var.platform_network_cidr != null && var.platform_subnet_cidr != null && length(var.platform_admin_cidrs) > 0
      error_message = "Review network ranges and administrative access before provisioning."
    }
  }
}

resource "hcloud_network_subnet" "platform" {
  count = local.platform_enabled ? 1 : 0

  network_id   = hcloud_network.platform[0].id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = var.platform_subnet_cidr
}

resource "hcloud_placement_group" "platform_control_planes" {
  count = local.platform_enabled ? 1 : 0

  name = "platform-control-planes"
  type = "spread"
}

resource "hcloud_firewall" "platform_control_planes" {
  count = local.platform_enabled ? 1 : 0

  name = "platform-control-planes"
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = var.platform_admin_cidrs
  }
  rule {
    direction  = "in"
    protocol   = "udp"
    port       = "41641"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
  rule {
    direction  = "in"
    protocol   = "icmp"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
}

resource "hcloud_server" "platform_control_plane" {
  for_each = var.platform_control_planes

  name               = each.value.name
  server_type        = "cx33"
  location           = "hel1"
  image              = "ubuntu-24.04"
  ssh_keys           = each.value.ssh_keys
  placement_group_id = hcloud_placement_group.platform_control_planes[0].id
  firewall_ids       = [hcloud_firewall.platform_control_planes[0].id]
  delete_protection  = true
  rebuild_protection = true
  labels             = { managed-by = "opentofu", scope = "platform", role = "control-plane" }

  network {
    network_id = hcloud_network.platform[0].id
    subnet_id  = hcloud_network_subnet.platform[0].id
    ip         = each.value.private_ip
    alias_ips  = []
  }

  depends_on = [hcloud_network_subnet.platform]

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [image, ssh_keys]
  }
}

resource "hcloud_server_network" "platform_existing_control_plane" {
  for_each = var.platform_existing_control_plane_networks

  server_id = each.value.server_id
  subnet_id = hcloud_network_subnet.platform[0].id
  ip        = each.value.private_ip
  alias_ips = []

  lifecycle {
    prevent_destroy = true
    precondition {
      condition     = local.platform_enabled
      error_message = "The platform network must be configured before attaching existing control planes."
    }
  }
}

output "platform_control_planes" {
  value = {
    for key, server in hcloud_server.platform_control_plane : key => {
      id         = server.id
      name       = server.name
      ipv4       = server.ipv4_address
      private_ip = var.platform_control_planes[key].private_ip
    }
  }
}

output "platform_network_id" {
  value = try(hcloud_network.platform[0].id, null)
}

output "platform_control_plane_placement_group_id" {
  value = try(hcloud_placement_group.platform_control_planes[0].id, null)
}

output "platform_existing_control_plane_networks" {
  value = {
    for key, attachment in hcloud_server_network.platform_existing_control_plane : key => {
      server_id = attachment.server_id
      ip        = attachment.ip
      mac       = attachment.mac_address
    }
  }
}
