# Cloudflare — llunde.no zone

The `llunde.no` zone lives on Cloudflare and is currently managed **out-of-band** (dashboard/API). [ADR 012](decisions/012-cloudflare-strategy.md) sets the target: DNS-only (grey-cloud) through the phase-2 cutover, the zone brought under OpenTofu as a Should, and proxy/WAF (orange-cloud) deliberately deferred to phase 4 because it changes real-IP handling end-to-end.

- Account: `8786559b30fcebd08d0c594b6e899eef` · Zone (`llunde.no`): `4ae54b24fc4140d4d1c450491645f1c8`

## Current records (reality, 2026-08)

| Record | Points at | Status |
|---|---|---|
| `llunde.no`, `www.llunde.no` | proxied CNAME → llunde tunnel `ed8abcdb-508c-4d61-85b7-bc3127510e4b` | **Retired by phase 2** |
| `parser.llunde.no`, `external.llunde.no` | proxied CNAME → pyparser tunnel | Untouched until phase 3 — see `docs/pyparser/CLOUDFLARE.md` |

The llunde tunnel fronts the old stack (cloudflared → nginx on the origin's internal network; the Hetzner firewall is SSH-only, so nothing on 80/443 reaches the box directly today). This is why the [phase-2 cutover](init/plans/phase-2/README.md) is a DNS change, not just a process swap: open 80/443 in the new firewall, repoint `llunde.no`/`www` to direct A/AAAA records (grey-cloud) for `llunde-01`, add `api.llunde.no`, let Caddy take Let's Encrypt from there, then delete the llunde tunnel.

## ⚠️ While tunnels are live

Do not rotate tunnel tokens. A tunnel's token embeds its secret — regenerating it drops the live connectors (site outage) until the new token is deployed. This constraint dies for llunde with the phase-2 tunnel retirement; it remains real for pyparser.

## Bringing the zone into OpenTofu (phase 3, step 2)

Scheduled: [phase-3 tasks](init/plans/phase-3/tasks.md) step 2 ([ADR 012](decisions/012-cloudflare-strategy.md)). When the zone moves under `tofu/`: import the existing records rather than recreating, keep the pyparser tunnel records pinned with `lifecycle { ignore_changes }` on anything embedding secrets, and mind the Cloudflare provider v4→v5 restructuring (`cloudflare_record` → `cloudflare_dns_record`, Zero Trust resources renamed) noted in `docs/pyparser/CLOUDFLARE.md`.
