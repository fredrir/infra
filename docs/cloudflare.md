# Cloudflare — llunde.no zone

The zone is **under OpenTofu** (ADR 012 part 2, executed phase-3 step 2):
`tofu/modules/cloudflare/main.tf` is the source of truth for every record, and
`tofu -chdir=tofu plan` is the audit — a clean plan means the zone matches git.
Dashboard edits are drift and get reverted by the next apply.

- Account `8786559b30fcebd08d0c594b6e899eef` · Zone (`llunde.no`) `4ae54b24fc4140d4d1c450491645f1c8`
- Token: `CLOUDFLARE_API_TOKEN`, Doppler `llunde/ops`, laptop-only — never a host
  or CI secret.

> **✅ Phase-4 edge cutover executed 2026-08-13** (ADR 017, [runbook §7](runbook.md)).
> `llunde.no`/`www`/`api` are proxied CNAMEs on the llunde tunnel, 80/443 are
> closed at both the Hetzner firewall and the NixOS firewall, and **no record in
> this zone resolves to a host address**. The estate has zero public inbound.

## Shape

| Names | Served by |
|---|---|
| `llunde.no`, `www.llunde.no`, `api.llunde.no` | proxied CNAMEs → llunde tunnel `c0cdd9b5-fa97-42a1-bca7-95da236ea949` on **llunde-01** |
| `parser.llunde.no`, `external.llunde.no` | proxied CNAMEs → pyparser tunnel `e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9` on **llunde-parser** |
| SES DKIM / SPF / DMARC | grey, imported verbatim during the phase-3 audit |

`hansteen.dev` (portfolio) is portfolio's **own zone with its own tunnel in THIS
SAME Cloudflare account**, managed by its own terraform — permanently outside
this repo's scope (ADR 016, [phase-3 mapping](research/phase-3-mapping.md)).

> ⚠️ **A Cloudflare *Tunnel* API permission cannot be scoped to one tunnel.** It
> is account-wide, and this account holds three: `llunde`,
> `hansteen-portfolio-origin` and `pyparser-review`. Verified 2026-08-12 — a
> token with that permission reads and rewrites the ingress of all three,
> straight across the ADR 016 tenant boundary. So any token carrying it belongs
> on the owner's laptop and **never on a host**: a host-resident copy hands the
> internet-facing service user control of the other tenants' front doors. The
> DNS-01 token in sops is `Zone:DNS:Edit` on llunde.no **only**, for exactly
> this reason.

Zone-level posture, verified 2026-08-12 against the already-proxied
`parser.llunde.no`: **Always Use HTTPS on**, `CF-Ray` on every proxied response
(what the `blackbox-cfray` probe asserts), and **no HSTS** — so a break-glass TLS
error is click-through-able, not a hard failure.

## The two llunde tunnels are not symmetric

| | llunde `c0cdd9b5` | pyparser `e77d6ebf` |
|---|---|---|
| Host | llunde-01 | llunde-parser |
| Connector | `Network=host`, dials Caddy on loopback `:8085` | shared podman network, dials service names |
| Image | **digest-pinned** — the front door never rides a floating tag | `:latest` (predates ADR 017) |
| Ingress map | **tofu** (`cloudflare_zero_trust_tunnel_cloudflared_config`) | dashboard |

The `Network=host` choice is load-bearing and does **not** transfer between them:
a podman-networked container's `localhost` is its own, so pyparser's shape would
never reach Caddy's 127.0.0.1 listener (phase-3.5 cutover lesson).

Day-to-day tunnel operations — connector health, reading the live ingress map,
digest bumps, failure modes — are [runbook §13](runbook.md).

## ⚠️ While tunnels are live

**Never rotate a tunnel token casually.** A tunnel's token embeds its secret —
regenerating it drops the live connectors (site outage) until the new token is
deployed. This is permanent for both tunnels. The phase-2-era note that the
constraint would "die with the llunde tunnel retirement" referred to the
**retired** `ed8abcdb-508c-4d61-85b7-bc3127510e4b` tunnel, which fronted the
pre-NixOS stack and was deleted at the phase-2 cutover; ADR 017 brought a tunnel
back on purpose, so the rule stands.

## Certificates

Caddy holds Let's Encrypt certificates for the three hostnames and renews them
over **DNS-01**, using a separate host-scoped `Zone:DNS:Edit` token in sops —
never the ops token, which also carries account-wide tunnel rights. With 80/443
closed there is no http-01 or tls-alpn-01 path; the running TLS policy carries a
`dns` challenge and nothing else, for both the Let's Encrypt and ZeroSSL
issuers. Proven 2026-08-13 by issuing a real certificate for a name with no DNS
record at all.

Warm certificates are what keep break-glass fast: re-open the ports, flip DNS
back to A records, and the certificates are already valid rather than racing
Let's Encrypt during an outage. See ADR 017 and runbook §13.
