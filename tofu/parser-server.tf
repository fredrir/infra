# The pyparser production server (CCX23; web ingress via Cloudflare tunnel, SSH
# via tailnet, zero public inbound — ADR 015). Adopted via terraform import; see
# docs/pyparser/PROD.md. Args mirror the live box so `plan` stays "No changes".
module "parser" {
  source = "./modules/hetzner"

  name          = "llunde-parser"
  server_type   = "ccx23"
  location      = "hel1"
  image         = "ubuntu-24.04"
  firewall_name = "pyparser-parser-fw"

  # No public inbound; re-open procedure in runbook §11. Same provider quirk as
  # tofu/llunde-firewall.tf — hcloud cannot DELETE the last rule (its update
  # omits the rules field and silently no-ops) — so zero was set out-of-band:
  #   hcloud firewall replace-rules pyparser-parser-fw --rules-file <(echo '[]')
  # NOT `<([])`: `[]` is not a command, so the process substitution hands hcloud
  # an EMPTY file while the shell still exits 0.
  inbound_rules = []
  # delete_protection / rebuild_protection default to true in the module.
}
