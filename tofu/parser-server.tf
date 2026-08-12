# The pyparser production server (CCX23; web ingress via Cloudflare tunnel,
# SSH via tailnet, zero public inbound — ADR 015). Adopted via terraform
# import; see docs/pyparser/PROD.md. Args mirror the live box exactly so
# `plan` stays a clean "No changes".
module "parser" {
  source = "./modules/hetzner"

  name          = "llunde-parser"
  server_type   = "ccx23"
  location      = "hel1"
  image         = "ubuntu-24.04"
  firewall_name = "pyparser-parser-fw"

  # No public inbound at all: web ingress is the Cloudflare tunnel (outbound),
  # SSH rides the tailnet (ADR 015). Re-open procedure: runbook §11.
  # Provider quirk: hcloud cannot DELETE the last rule (its update omits the
  # rules field, silently no-oping) — going to zero was done once out-of-band
  # with `hcloud firewall replace-rules pyparser-parser-fw --rules-file <(echo '[]')`.
  # (`<([])` as first recorded is a typo: `[]` is not a command, so the process
  # substitution hands hcloud an EMPTY file while the shell still exits 0.)
  inbound_rules = []
  # delete_protection / rebuild_protection default to true in the module.
}
