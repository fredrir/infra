# The pyparser production server (CCX23, behind a Cloudflare tunnel → SSH-only).
# Adopted via terraform import; see ../README.md and PROD.md. Args mirror the
# live box exactly so `plan` stays a clean "No changes".
module "server" {
  source = "../modules/hetzner"

  name          = "llunde-parser"
  server_type   = "ccx23"
  location      = "hel1"
  image         = "ubuntu-24.04"
  firewall_name = "pyparser-parser-fw"

  inbound_rules = [
    { port = "22", description = "SSH" },
  ]
  # delete_protection / rebuild_protection default to true in the module.
}
