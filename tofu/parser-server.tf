module "parser" {
  source = "./modules/hetzner"

  name          = "fredrir-04"
  server_type   = "ccx23"
  location      = "hel1"
  image         = "ubuntu-24.04"
  firewall_name = "pyparser-parser-fw"

  inbound_rules = []
}
