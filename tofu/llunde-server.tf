# Fully-managed server, create-mode — deliberately NOT the adoption pattern
# tofu/modules/hetzner uses (ADR 001/002). The existing box came under management
# via the import below, then was wiped and reinstalled with nixos-anywhere.
import {
  to = hcloud_server.llunde_01
  id = "132168416"
}

resource "hcloud_server" "llunde_01" {
  name        = "llunde-01"
  server_type = "cpx22"
  # hel1 inferred from the box's 46.62.x address; confirm with
  # `hcloud server describe 132168416`.
  location = "hel1"

  # Historical attribute of the imported server; the OS is replaced by
  # nixos-anywhere (ADR 001), so the image is never managed here. The ONLY
  # lifecycle concession — no prevent_destroy, no blanket ignore_changes.
  lifecycle {
    ignore_changes = [image]
  }

  image = "ubuntu-24.04"

  # On since bring-up; off only while the wipe/reinstall cycle needed to run.
  delete_protection  = true
  rebuild_protection = true
}
