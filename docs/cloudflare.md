# Cloudflare — llunde.no zone

The zone is **under OpenTofu** (ADR 012): `tofu/modules/cloudflare/main.tf` is
the source of truth for every record, and `tofu -chdir=tofu plan` is the audit —
a clean plan means the zone matches git. Dashboard edits are drift and get
reverted by the next apply.

- Account `8786559b30fcebd08d0c594b6e899eef` · Zone (`llunde.no`)
  `4ae54b24fc4140d4d1c450491645f1c8`
- Token: `CLOUDFLARE_API_TOKEN`, Doppler `llunde/ops`, laptop-only. Scope:
  `Zone:DNS:Edit` on llunde.no plus `Account:Cloudflare Tunnel:Read` and
  `:Edit`. The read half is not optional — a plan refreshes the tunnel config
  resource, so every run needs it.

Every name is a proxied CNAME onto a tunnel; **no record resolves to a host
address**, and 80/443 are closed at both the Hetzner and the NixOS firewall
(ADR 017, [runbook](runbook.md)).

| Names | Served by |
|---|---|
| `llunde.no`, `www.llunde.no`, `api.llunde.no` | proxied CNAMEs → llunde tunnel `c0cdd9b5-fa97-42a1-bca7-95da236ea949` on **llunde-01** |
| `parser.llunde.no`, `external.llunde.no` | proxied CNAMEs → pyparser tunnel `e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9` on **llunde-parser** |
| SES DKIM / SPF / DMARC | grey, imported verbatim |

`hansteen.dev` (portfolio) is portfolio's **own zone with its own tunnel in this
same Cloudflare account**, managed by its own terraform — permanently outside
this repo's scope (ADR 016).

> ⚠️ **A Cloudflare *Tunnel* API permission cannot be scoped to one tunnel.**
> Verified 2026-08-12: it is account-wide, and this account holds three —
> `llunde`, `hansteen-portfolio-origin`, `pyparser-review` — so a token carrying
> it reads and rewrites the ingress of all three, straight across the ADR 016
> tenant boundary. Such a token belongs on the owner's laptop and **never on a
> host**: a host-resident copy hands the internet-facing service user control of
> the other tenants' front doors. That is why the DNS-01 token in sops is
> `Zone:DNS:Edit` on llunde.no **only**.

Zone posture, verified 2026-08-12: **Always Use HTTPS on**, `CF-Ray` on every
proxied response (what the `blackbox-cfray` probe asserts), and **no HSTS** — so
a break-glass TLS error is click-through-able, not a hard failure.

## The two llunde tunnels are not symmetric

| | llunde `c0cdd9b5` | pyparser `e77d6ebf` |
|---|---|---|
| Host | llunde-01 | llunde-parser |
| Connector | `Network=host`, dials Caddy on loopback `:8085` | shared podman network, dials service names |
| Image | **digest-pinned** — the front door never rides a floating tag | `:latest` (predates ADR 017) |
| Ingress map | **tofu** (`cloudflare_zero_trust_tunnel_cloudflared_config`) | dashboard |

`Network=host` is load-bearing and does **not** transfer: a podman-networked
container's `localhost` is its own, so pyparser's shape would never reach
Caddy's 127.0.0.1 listener.

Connector health, reading the live ingress map, digest bumps and failure modes
are in [docs/runbook.md](runbook.md).

## ⚠️ Never rotate a tunnel token casually

A tunnel's token embeds its secret: regenerating it drops the live connectors
(site outage) until the new token is deployed. Permanent, and true of both
tunnels.

## Certificates

Caddy holds Let's Encrypt certificates for the three llunde hostnames and renews
them over **DNS-01**, using a separate host-scoped `Zone:DNS:Edit` token in sops
— never the ops token, which also carries account-wide tunnel rights. With
80/443 closed there is no http-01 or tls-alpn-01 path; the running TLS policy
carries a `dns` challenge and nothing else, for both the Let's Encrypt and
ZeroSSL issuers. Proven 2026-08-13 by issuing a real certificate for a name with
no DNS record at all.

Warm certificates are what keep break-glass fast: re-open the ports, flip DNS
back to A records, and the certificates are already valid rather than racing
Let's Encrypt during an outage (ADR 017, [runbook](runbook.md)).
