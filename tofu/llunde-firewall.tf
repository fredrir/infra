# NO inbound rules: zero public inbound (ADR 017). 22 is closed in favour of
# tailnet SSH (ADR 015); 80/443 closed once llunde.no moved onto the tunnel.
# Outbound is unrestricted, as the tunnel and every other egress path need.
#
# ⚠️ `tofu apply` CANNOT empty this and will not say so: the hcloud provider
# cannot delete a firewall's last rules — its update omits the rules field,
# no-ops, and prints "Apply complete". Go to zero out-of-band, then reconcile
# with `tofu apply -refresh-only`:
#   echo "[]" | hcloud firewall replace-rules --rules-file - llunde-fw
# Same trap as tofu/parser-server.tf. Re-opening (break-glass) DOES work through
# the provider, faster via `hcloud firewall add-rule` (runbook §7.3).
resource "hcloud_firewall" "llunde_fw" {
  name = "llunde-fw"
}

resource "hcloud_firewall_attachment" "llunde_fw" {
  firewall_id = hcloud_firewall.llunde_fw.id
  server_ids  = [hcloud_server.llunde_01.id]
}
