# ADR 012: Cloudflare — DNS-Only Now, Zone into Tofu, Proxy Deferred

**Status**: Accepted · **Date**: 2026-08-09

## Context

The `llunde.no` zone lives on Cloudflare, historically managed by hand with out-of-band docs. The owner's target architecture sketches Cloudflare as an edge layer (WAF, rate limiting, bot filtering, DDoS) — which means orange-cloud proxying. But proxying changes real-IP handling materially: the origin sees CF edge addresses, and every consumer of the client IP downstream is affected.

That is not hypothetical here. The backend's external security review found and fixed an X-Forwarded-For trust bug (rate-limit buckets mintable by spoofed headers); the fix trusts the rightmost XFF entry appended by *our own* proxy, and its integrity is explicitly conditional on the topology contract. Putting Cloudflare in front adds a hop: CF appends the client IP, Caddy appends CF's IP — and unless **Caddy** is configured with `trusted_proxies` for Cloudflare's published ranges and forwards the resolved client IP, the backend's rate-limit keys and audit IPs become Cloudflare datacenter addresses.

## Decision

Three-part strategy:

1. **Phase 2 cutover runs DNS-only (grey-cloud).** Caddy terminates TLS with Let's Encrypt normally, sees real client IPs, and the reviewed XFF chain (client → Caddy → app) holds with zero new configuration. Fewest variables while the new world goes live.
2. **The zone moves under OpenTofu** (cloudflare provider, `tofu/modules/cloudflare/`) as a **Should** — DNS is the one genuinely shared surface across services, and codifying it kills "what did I click in the dashboard" drift. Records for `llunde.no`, `api.llunde.no`, `parser.llunde.no`.
3. **Orange-cloud proxy/WAF is phase 4, done deliberately**: enable proxying, configure Caddy `trusted_proxies` with CF ranges, restrict the Hetzner firewall's 80/443 to CF ranges if desired, and **verify the CF → Caddy → app chain end-to-end** — the backend's rotating-XFF test exists precisely to prove buckets cannot be minted.

## Alternatives considered

- **Orange-cloud at cutover** — maximum protection day one, but stacks an untested proxy hop onto a brand-new host during the riskiest window, with a known bug class waiting. Rejected for sequencing, not for substance.
- **No Cloudflare involvement at all** — forfeits free DDoS absorption and the future WAF option while the zone is already there. Rejected.
- **Cloudflare Tunnel for ingress** — couples ingress to a daemon and conflicts with the one-overlay management decision ([ADR 008](008-tailscale-management.md)). Rejected for this box.

## Consequences

- Until phase 4, origin IP is public in DNS — acceptable for the zero-users phase; the Hetzner firewall still admits only 80/443 (+break-glass 22).
- Phase 4 inherits a written verification procedure instead of a vibe: the chain test is named in this ADR and the phase-4 plan.
- Managing the zone in tofu makes the eventual grey→orange flip a reviewed one-line diff, not a dashboard click.
