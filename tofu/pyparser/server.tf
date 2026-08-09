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

  # No public inbound at all: web ingress is the Cloudflare tunnel (outbound),
  # SSH rides the tailnet (ADR 015). Re-open procedure: runbook §11.
  inbound_rules = []
  # delete_protection / rebuild_protection default to true in the module.
}
