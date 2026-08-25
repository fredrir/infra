resource "hcloud_server" "this" {
  name        = var.name
  server_type = var.server_type
  location    = var.location
  image       = var.image
  ssh_keys    = var.ssh_keys

  delete_protection  = var.delete_protection
  rebuild_protection = var.rebuild_protection

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [image, user_data, ssh_keys, keep_disk, backups]
  }
}

resource "hcloud_firewall" "this" {
  name = var.firewall_name

  dynamic "rule" {
    for_each = var.inbound_rules
    content {
      direction   = "in"
      protocol    = "tcp"
      port        = rule.value.port
      source_ips  = ["0.0.0.0/0", "::/0"]
      description = rule.value.description
    }
  }
}

resource "hcloud_firewall_attachment" "this" {
  firewall_id = hcloud_firewall.this.id
  server_ids  = [hcloud_server.this.id]
}
