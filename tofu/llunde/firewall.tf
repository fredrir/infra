# Target rule set: 22 (break-glass SSH until Tailscale is proven, ADR 008),
# 80/443 (Caddy, ADR 006). The live firewall is currently SSH-only
# (research/current-state.md) — 80/443 open when phase 2 applies this.
resource "hcloud_firewall" "llunde_fw" {
  name = "llunde-fw"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

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
