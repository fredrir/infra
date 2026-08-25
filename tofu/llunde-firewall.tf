resource "hcloud_firewall" "llunde_fw" {
  name = "llunde-fw"
}

resource "hcloud_firewall_attachment" "llunde_fw" {
  firewall_id = hcloud_firewall.llunde_fw.id
  server_ids  = [hcloud_server.llunde_01.id]
}
