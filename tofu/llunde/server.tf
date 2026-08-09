# Fully-managed server (create-mode — deliberately NOT the adoption pattern the
# old root and tofu/modules/hetzner use; ADR 001/002). The existing box is
# brought under management via the declarative import below and then wiped and
# reinstalled with nixos-anywhere in phase 2.
import {
  to = hcloud_server.llunde_01
  id = "132168416"
}

resource "hcloud_server" "llunde_01" {
  name        = "llunde-01"
  server_type = "cpx22"
  # hel1 inferred from the box's 46.62.x address — confirm against
  # `hcloud server describe 132168416` when the phase-2 plan runs.
  location = "hel1"

  # Historical attribute of the imported server; the OS is replaced by
  # nixos-anywhere (ADR 001), so the image is never managed here.
  # This is the ONLY lifecycle concession — no prevent_destroy, no blanket
  # ignore_changes.
  lifecycle {
    ignore_changes = [image]
  }

  image = "ubuntu-24.04"

  # On since the phase-2 cutover gate closed (2026-08-09); they were off only
  # to keep the wipe/reinstall cycle unblocked during bring-up.
  delete_protection  = true
  rebuild_protection = true
}
