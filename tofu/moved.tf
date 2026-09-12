moved {
  from = module.server
  to   = module.parser
}

moved {
  from = module.parser
  to   = module.worker_04
}

moved {
  from = hcloud_server.llunde_01
  to   = hcloud_server.control_05
}

moved {
  from = hcloud_firewall.llunde_fw
  to   = hcloud_firewall.control_05
}

moved {
  from = hcloud_firewall_attachment.llunde_fw
  to   = hcloud_firewall_attachment.control_05
}
