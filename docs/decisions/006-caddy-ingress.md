# ADR 006: Caddy as the Single Public Ingress

**Status**: Accepted · **Date**: 2026-08-09

## Context

The old box terminated TLS with nginx+certbot configured in place. The new world needs one public entry point for multiple decoupled services on the same host — `llunde.no` (frontend) and `api.llunde.no` (backend) today, more later — with TLS that manages itself and configuration that lives in the repo. Only ingress should be publicly reachable; everything else is loopback-published per [ADR 005](005-per-service-users-isolation.md).

## Decision

**Caddy**, run as a Quadlet container under the dedicated `edge` user, is the only service publishing public ports (80/443).

- One Caddyfile (Nix-templated in `modules/ingress/`) routes:
  - `llunde.no` → `127.0.0.1:8081` (frontend container)
  - `api.llunde.no` → `127.0.0.1:8080` (backend)
- **Automatic TLS via Let's Encrypt** with the standard HTTP/TLS-ALPN challenges — possible because Cloudflare stays **DNS-only** for now ([ADR 012](012-cloudflare-strategy.md)). Certificate storage persists on a volume so reissues don't burn rate limits across container restarts.
- Caddy appends the client IP via `X-Forwarded-For`; the backend trusts exactly the rightmost entry (its `useLastProxy()` configuration from the backend's phase-2 hardening). This chain is only sound while the app port is unreachable except through Caddy — guaranteed here by loopback-only publishing, and re-examined when Cloudflare proxying arrives in phase 4.
- The operational endpoints (`/metrics`, `/health`, `/ready`) are **not routed** by the public Caddyfile — they stay reachable only over the tailnet ([ADR 013](013-observability-host-scope.md)), which satisfies the backend phase-1 review finding that they must never be public.

## Alternatives considered

- **nginx + certbot** — the old stack: two moving parts (server + renewal cron) doing what Caddy does in one, with config that historically lived only on the box.
- **Traefik** — label/discovery machinery earns its keep with many dynamic containers; this host has a handful of static routes.
- **Cloudflare Tunnel as ingress** — couples all ingress to Cloudflare and complicates local/tailnet access; CF's edge role is deferred to phase 4 deliberately.

## Consequences

- Adding a service's public route = one Caddyfile block + its loopback port; no new TLS thinking, ever.
- The `edge` user needs `ip_unprivileged_port_start=80` (declared in the profile) — recorded in [ADR 005](005-per-service-users-isolation.md).
- Brief cutover downtime is inherent: old nginx dies with the wipe, Caddy comes up with the new world ([ADR 001](001-nixos-declarative-host.md)); accepted since the old stack has no users.
- When phase 4 turns on Cloudflare proxying, Caddy gains `trusted_proxies` for CF ranges and the real-IP chain gets re-verified end-to-end — tracked in the phase-4 plan, not here.
