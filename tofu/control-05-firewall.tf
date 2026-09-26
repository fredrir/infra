resource "hcloud_firewall" "control_05" {
  name = "fredrir-05-fw"

  rule {
    direction  = "in"
    protocol   = "udp"
    port       = "41641"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
}

resource "hcloud_firewall_attachment" "control_05" {
  firewall_id = hcloud_firewall.control_05.id
  server_ids  = [hcloud_server.control_05.id]
}
