# ADR 017: llunde.no Ingress Moves to a Cloudflare Tunnel

**Status**: Accepted · **Date**: 2026-08-09 (phase-4 re-evaluation) · Amends [ADR 012](012-cloudflare-strategy.md) part 3; supersedes its tunnel rejection

## Context

ADR 012 rejected tunnel ingress for llunde-01 "for this box" on two grounds: it coupled ingress to a daemon nobody here had operated, and it muddied the management-path picture (ADR 008's one-overlay argument). It scheduled orange-cloud proxying as the phase-4 edge instead. Both grounds have since dissolved:

- **The estate operates tunnels in production.** pyparser's tunnel has served `parser.llunde.no` for months; portfolio runs its own; phase 3.5 re-expressed cloudflared as a declared quadlet with its token in the standard secrets flow. The "unproven daemon" is now the estate's *most* proven ingress.
- **Management is fully decoupled from ingress** (ADR 015, executed): SSH rides the tailnet regardless of how web traffic arrives. A tunnel used purely for ingress reintroduces no management path.
- **The uniformity principle** (ADR 016) now cuts toward tunnels: llunde-parser has zero public inbound ports; llunde-01 keeping 80/443 open is the estate's last public surface.
- The owner's phase-2 instinct — "shouldn't we use cloudflare tunnels? for all services?" — was deferred, not rejected. This ADR is that discussion, held as promised, concluding in favor.

## Decision

**`llunde.no`, `www.llunde.no`, and `api.llunde.no` are served through a named Cloudflare Tunnel** terminated by a `cloudflared` quadlet under the `edge` user — **`Network=host`**, because it dials Caddy on loopback (a podman-networked container's localhost is its own; pyparser's shared-network cloudflared is a different shape that does not transfer). Consequences by layer:

- **DNS** (tofu, ADR 012's zone): the direct A/AAAA records become proxied CNAMEs to the tunnel — a reviewed diff.
- **Firewall**: 80/443 close (tofu + NixOS) → zero public inbound on the estate.
- **TLS**: Cloudflare's edge terminates public TLS; Caddy serves plain HTTP on loopback to the connector. Let's Encrypt management for these hosts retires.
- **Caddy stays** as the internal router: vhost routing, ops-endpoint blocking (`/metrics` etc. — unchanged), www→apex redirect.
- **Real client IP**: the same work orange-cloud would have needed, with two traps the plan review pinned down — Caddy's tunnel listener makes `CF-Connecting-IP` the rightmost XFF client entry (unmapped, every bucket keys on cloudflared's 127.0.0.1), **scoped to that listener only** (on the still-open public path the header is attacker-supplied), and sets `X-Forwarded-Proto=https` (the plain-HTTP hop otherwise breaks Secure cookies and CSRF origin checks). The backend's rotating-XFF test proves the whole chain on a Host-overridden test hostname BEFORE the DNS flip. This is the exact bug class the backend's external review caught; the verification is not optional.
- **WAF/bot filtering**: available on tunneled hostnames exactly as on orange-cloud — the original phase-4 goal arrives as a side effect.
- **The tunnel token** is a llunde-01 sops secret (env form), never rotated casually (the standing tunnel rule).
- **The connector image is pinned by digest** (second-opinion requirement): pyparser's `:latest` cloudflared predates this ADR and must not extend to the front door — a broken upstream release would take the site down; updates are deliberate digest bumps. ~~The alternative of keeping certs warm via DNS-01 (custom Caddy build with the CF plugin) was considered and declined — break-glass consciously pays the documented TLS-outage window instead of carrying that complexity.~~ **Reversed 2026-08-12 — see the amendment below.**

## Amendment (2026-08-12): certificates renew via DNS-01

The decision above declined DNS-01 and accepted stale certificates. At execution the owner reversed it, and the reversal is recorded here rather than left implicit in a commit.

**What changed.** Caddy runs an image built by this repo (`images/caddy`) with `caddy-dns/cloudflare` compiled in, and `acme_dns cloudflare` in the global block makes DNS-01 the default challenge for all three names. A host-scoped `Zone:DNS:Edit` token reaches it via sops.

**Why the original reasoning didn't survive contact.** It weighed only complexity. Two things it did not price:

- **The window is not bounded by us.** Break-glass would have meant re-opening 80/443 *and then* racing Let's Encrypt — issuance, plus whatever failed-validation rate limit the preceding weeks of doomed renewals had consumed. Warm certs make the grey-flip a DNS change and nothing else.
- **The front door was never actually pinned.** It ran the floating `docker.io/library/caddy:2` tag, held still only by quadlet's `Pull=missing` and a warm image cache. Building our own image was already the fix for that; the DNS-01 plugin rides along at no extra structural cost.

**The residual this buys, stated plainly.** A `Zone:DNS:Edit` credential now lives on the internet-facing host. A compromise of `edge` is takeover of the whole `llunde.no` zone — **including MX/SPF/DKIM, i.e. email** — because Cloudflare cannot scope DNS-edit below the zone. That is strictly worse than the tunnel token already there, and it is accepted knowingly. It is *not* the laptop's ops token, which additionally carries account-wide Cloudflare Tunnel rights over the portfolio and pyparser tenants' tunnels (ADR 016 boundary) and must never reach a host.

**A trap this created.** With `acme_dns cloudflare` in the config, the reconcile map's pre-restart `caddy validate` must run the **container image**, not `pkgs.caddy`. A nixpkgs caddy carrying the plugin would happily validate a config the *running* image cannot parse, and reconcile would then restart the front door into a crash-loop. Validating with the binary that will serve is the only variant that fails safe; proven on llunde-01 (exit 0 on the current config, exit 1 on a DNS-01 config against a plugin-less image).

## Alternatives considered

- **Orange-cloud proxy** (ADR 012's sketch) — same real-IP work, keeps LE and a trivial proxy-off rollback, but leaves 80/443 open and origin IP public. The conservative middle; rejected in favor of the uniform zero-inbound estate.
- **Stay grey-direct** — zero work, no WAF, public origin. Rejected: the estate's last public ports for no benefit.

## Consequences

- Cloudflare becomes a hard availability dependency for llunde.no — as it already is for `parser.llunde.no` and `hansteen.dev`; two-thirds of the estate trusted it before this ADR.
- **Break-glass** (runbook): flip DNS back to direct A/AAAA (grey) + re-open 80/443 — two pre-staged tofu diffs; Caddy must be able to re-acquire LE certs in that mode, so the vhost config keeps the ACME path dormant rather than deleted.
- The uptime-monitoring caveat in ADR 013 gains force: external checks hit CF's edge, not the origin; origin-level health lives on the tailnet (workstream O).
- ADR 012's parts 1–2 (grey-at-cutover history, zone-in-tofu) stand; part 3 (orange-cloud as the phase-4 edge) is superseded by this ADR.
