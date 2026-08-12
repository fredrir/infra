# NO inbound rules: zero public inbound (phase-4 E6, ADR 017). 22 closed in
# phase 3 (ADR 015, tailnet SSH); 80/443 closed at the E5/E6 cutover once
# llunde.no moved onto the tunnel. Outbound is unrestricted, which is what the
# tunnel and every other egress path need.
#
# ⚠️ Emptying this by `tofu apply` DOES NOT WORK and does not say so: the hcloud
# provider cannot delete a firewall's last rules — the update omits the rules
# field entirely, silently no-ops, and prints "Apply complete". Going to zero
# was done out-of-band, then reconciled with `tofu apply -refresh-only`:
#   echo "[]" | hcloud firewall replace-rules --rules-file - llunde-fw
# Same trap, same workaround, as tofu/parser-server.tf. Re-opening (break-glass)
# DOES work through the provider, and faster through `hcloud firewall add-rule`
# — runbook §7.3.
resource "hcloud_firewall" "llunde_fw" {
  name = "llunde-fw"
}

resource "hcloud_firewall_attachment" "llunde_fw" {
  firewall_id = hcloud_firewall.llunde_fw.id
  server_ids  = [hcloud_server.llunde_01.id]
}
