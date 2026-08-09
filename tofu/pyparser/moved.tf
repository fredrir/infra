# Wrap the previously root-level hcloud resources into module.server WITHOUT a
# recreate. `plan` should report these as moves + "No changes". Safe to delete
# after the first apply records the moves into state.
moved {
  from = hcloud_server.parser
  to   = module.server.hcloud_server.this
}

moved {
  from = hcloud_firewall.parser
  to   = module.server.hcloud_firewall.this
}

moved {
  from = hcloud_firewall_attachment.parser
  to   = module.server.hcloud_firewall_attachment.this
}
