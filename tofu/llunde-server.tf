import {
  to = hcloud_server.llunde_01
  id = "132168416"
}

resource "hcloud_server" "llunde_01" {
  name        = "llunde-01"
  server_type = "cpx22"
  location    = "hel1"

  lifecycle {
    ignore_changes = [image]
  }

  image = "ubuntu-24.04"

  delete_protection  = true
  rebuild_protection = true
}
