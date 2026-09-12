module "worker_04" {
  source = "./modules/hetzner"

  name          = "fredrir-04"
  server_type   = "ccx23"
  location      = "hel1"
  image         = "ubuntu-26.04"
  firewall_name = "fredrir-04-fw"

  inbound_rules = []
}
