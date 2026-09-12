import {
  to = hcloud_server.control_05
  id = "132168416"
}

resource "hcloud_server" "control_05" {
  name        = "fredrir-05"
  server_type = "cpx22"
  location    = "hel1"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [image]
  }

  image = "ubuntu-26.04"

  delete_protection  = true
  rebuild_protection = true
  placement_group_id = var.platform_existing_control_plane_spread_enabled ? hcloud_placement_group.platform_control_planes[0].id : null
}
