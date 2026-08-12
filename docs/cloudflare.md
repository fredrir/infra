# Cloudflare — llunde.no zone

The zone is **under OpenTofu** (ADR 012 part 2, executed phase-3 step 2):
`tofu/modules/cloudflare/main.tf` is the source of truth for every record, and
`tofu -chdir=tofu plan` is the audit — a clean plan means the zone matches git.
Dashboard edits are drift and get reverted by the next apply.

- Account `8786559b30fcebd08d0c594b6e899eef` · Zone (`llunde.no`) `4ae54b24fc4140d4d1c450491645f1c8`
- Token: `CLOUDFLARE_API_TOKEN`, Doppler `llunde/ops`, laptop-only — never a host
  or CI secret.

> **⏳ Phase-4 edge cutover is in flight** (ADR 017, [runbook §7](runbook.md)).
> Until E5 applies, `llunde.no`/`www`/`api` are **grey A + AAAA records straight
> at llunde-01** and Caddy terminates public TLS on open 80/443. E5 turns them
> into proxied CNAMEs on the llunde tunnel; E6 then closes 80/443, leaving the
> estate with zero public inbound. Everything below marked *(post-E5)* is the
> target, not today.

## Shape

| Names | Served by |
|---|---|
| `llunde.no`, `www.llunde.no`, `api.llunde.no` | direct A/AAAA → llunde-01, Caddy + Let's Encrypt · *(post-E5: proxied CNAMEs → llunde tunnel `c0cdd9b5-fa97-42a1-bca7-95da236ea949`)* |
| `parser.llunde.no`, `external.llunde.no` | proxied CNAMEs → pyparser tunnel `e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9` on **llunde-parser** |
| SES DKIM / SPF / DMARC | grey, imported verbatim during the phase-3 audit |

`hansteen.dev` (portfolio) runs its **own** tunnel in its own Cloudflare account
and is not managed here (ADR 016 slot boundary).

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
| Ingress map | dashboard *(post-E5: tofu, `cloudflare_zero_trust_tunnel_cloudflared_config`)* | dashboard |

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

Today Caddy holds Let's Encrypt certs for the three hostnames and renews them
over http-01 on the open port 80. *(post-E6: there is no http-01 path, so
renewal moves to **DNS-01** via a separate host-scoped `Zone:DNS:Edit` token in
sops — never the ops token. Warm certs are what keep break-glass fast: re-open
the ports, flip DNS back to A records, and the certs are already valid.)*
See ADR 017 and runbook §13.
