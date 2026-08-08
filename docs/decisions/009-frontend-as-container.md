# ADR 009: Frontend Deploys as a Container

**Status**: Accepted · **Date**: 2026-08-09

## Context

The frontend has been split out of the old monorepo into its own repo (github.com/fredrir/llunde-frontend). It serves from the **same box** as the backend, but the owner's requirement is that it be "as much completely separate from the other services" as possible — the decoupling of llunde frontend from llunde backend is a core goal of the whole restructure.

The old deploy workflow (preserved in [../research/current-state.md](../research/current-state.md)) built the Vite bundle in CI and **rsynced `dist/` over SSH** to `/opt/llunde/frontend-dist/` on the host, where nginx served it. That couples every frontend deploy to SSH credentials, a shared filesystem path, and host state that nothing declares.

## Decision

**The frontend is a container, exactly like every other service.** The frontend repo builds a minimal static-server image (the Vite `dist/` inside a small file server), pushes it to GHCR via the shared build workflow ([ADR 010](010-shared-ci-reusable-workflows.md)), and llunde-infra runs it as a Quadlet unit ([ADR 004](004-quadlet-own-abstraction.md)) under its own `llunde-frontend` user ([ADR 005](005-per-service-users-isolation.md)), publishing only `127.0.0.1:8081`. Caddy ([ADR 006](006-caddy-ingress.md)) proxies `llunde.no` to it.

The contract is identical to the backend's: an image name, a loopback port, a health path. Deploys are `podman auto-update` pulls — no SSH, no rsync, no host paths.

## Alternatives considered

- **rsync `dist/` to a Caddy-served volume** (the old pattern) — one fewer container, but re-couples deploys to SSH keys and host directories, and makes the frontend the only service with a bespoke deploy path. Rejected as exactly the coupling this restructure removes.
- **Cloudflare Pages** — recommended in round 1 (free CDN/TLS, zero server involvement) but the owner chose to keep the frontend on the Hetzner box, served by Caddy. The container model preserves most of the separation Pages would have given.
- **Caddy serving files baked into its own image** — merges frontend and ingress lifecycles; a frontend deploy would mean restarting the edge. Rejected.

## Consequences

- Every service on the box is "an image + a unit"; nothing else exists. The frontend deploys the moment its CI pushes and the auto-update timer fires.
- The frontend repo needs a Containerfile and a ~10-line caller workflow — listed as cross-repo tasks in the phase-2 plan.
- Vite build-time values (the old Turnstile site key pattern) are supplied as Doppler-sourced build args in the shared workflow, not GitHub secrets.
- Isolation is enforced by OS boundary: the frontend user's containers cannot reach Postgres/Valkey at all, not merely by network-config discipline.
- `llunde.no` and `api.llunde.no` remain split origins, matching the backend's ADR 007 cookie/CORS design unchanged.
