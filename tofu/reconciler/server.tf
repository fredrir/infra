provider "hcloud" {
  token = var.reconciler_hcloud_token
}

locals {
  name       = "fredrir-11"
  location   = "hel1"
  admin_keys = { for line in split("\n", trimspace(file("${path.module}/../../keys/admin_keys"))) : split(" ", line)[2] => line }
  labels     = { managed-by = "opentofu", role = "reconciler" }
  foreign    = [for server in data.hcloud_servers.project.servers : server.name if server.name != local.name]
}

data "hcloud_servers" "project" {}

resource "hcloud_ssh_key" "admin" {
  for_each   = local.admin_keys
  name       = each.key
  public_key = each.value
  labels     = local.labels
}

resource "hcloud_primary_ip" "reconciler" {
  name              = "${local.name}-ipv4"
  type              = "ipv4"
  location          = local.location
  auto_delete       = false
  delete_protection = true
  labels            = local.labels

  lifecycle {
    prevent_destroy = true
    precondition {
      condition     = length(local.foreign) == 0
      error_message = "The Hetzner project must hold only ${local.name}; use the reconciler project's token."
    }
  }
}

resource "hcloud_firewall" "reconciler" {
  name   = "${local.name}-fw"
  labels = local.labels

  dynamic "rule" {
    for_each = length(var.bootstrap_admin_cidrs) > 0 ? [var.bootstrap_admin_cidrs] : []
    content {
      direction   = "in"
      protocol    = "tcp"
      port        = "22"
      source_ips  = rule.value
      description = "bootstrap"
    }
  }
}

resource "hcloud_server" "reconciler" {
  name               = local.name
  server_type        = "cx33"
  location           = local.location
  image              = "ubuntu-26.04"
  ssh_keys           = [for key in hcloud_ssh_key.admin : key.id]
  firewall_ids       = [hcloud_firewall.reconciler.id]
  delete_protection  = true
  rebuild_protection = true
  labels             = local.labels

  public_net {
    ipv4_enabled = true
    ipv4         = hcloud_primary_ip.reconciler.id
    ipv6_enabled = false
  }

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [image, ssh_keys, user_data]
  }
}
