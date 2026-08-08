# The llunde.no production server. Adopted via `terraform import`.
#
# Ingress is a Cloudflare Tunnel (cloudflared dials OUT, like pyparser), so NO
# public web ports are needed — inbound is SSH only. The origin is unreachable
# on 80/443 from the internet; the site is served only through the tunnel.
module "server" {
  source = "../modules/hetzner-server"

  name          = "ubuntu-llunde"
  server_type   = "cpx22"
  location      = "hel1"
  firewall_name = "llunde-fw"

  inbound_rules = [
    { port = "22", description = "SSH" },
  ]
  # delete_protection / rebuild_protection default to true in the module.
}
