# Rule set: 80/443 only (Caddy, ADR 006). Public 22 closed in phase 3 —
# all SSH rides the tailnet (ADR 015); re-open procedure in runbook §11.
resource "hcloud_firewall" "llunde_fw" {
  name = "llunde-fw"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "80"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "443"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
}

resource "hcloud_firewall_attachment" "llunde_fw" {
  firewall_id = hcloud_firewall.llunde_fw.id
  server_ids  = [hcloud_server.llunde_01.id]
}
