# Phase 2 — task breakdown & delegation

Ground rule of this phase: **declarative files are authored in parallel; the live host is only ever touched serially by the lead** (tofu apply, nixos-anywhere, nixos-rebuild, DNS edits). Streams verify with `nix flake check` + `nixos-rebuild build --flake .#llunde-01` (build, never switch).

## Step 0 — Contracts & prerequisites (lead, sequential)

| # | Task | Detail |
|---|---|---|
| 0.1 | Port & user contract | Freeze in one doc comment: uids/users, loopback ports (backend 8080, frontend 8081), network names, cap values (JVM `-Xmx`, `shared_buffers`, `maxmemory`), image names + `:latest` autoupdate tags |
| 0.2 | Cross-repo dispatch | llunde-backend: Containerfile + build-image caller (its rescoped phase 3, executed in that repo); llunde-frontend: Containerfile + caller. Both push to GHCR — gate for streams that reference the images |
| 0.3 | sops scaffold | `.sops.yaml` + placeholder encrypted files; real values enter at step 2 when the host key exists |

## Step 1 — Parallel streams (sub-agents; flake-build green each)

| Stream | Owns | Builds | Follows | Done when |
|---|---|---|---|---|
| **A — Host & platform** | `modules/profiles/`, `modules/users/`, `modules/tailscale/`, `modules/secrets/`, `hosts/llunde-01/` | server profile (ssh, firewall, sysctls, unattended policy), users (subuid/subgid, linger), tailscale unit, sops wiring, disko final | ADR 001, 003, 005, 007, 008 | closure builds; options documented |
| **B — mkQuadlet & data services** | `modules/quadlet/`, `services/llunde-backend/` | mkQuadlet full (users/<uid> path, auto-update timer, daemon-reload hook); postgres/valkey/backend units with caps, private network, Doppler wrapper | ADR 004, 005, backend ADR 009/010 | closure builds; rendered unit text review |
| **C — Ingress & frontend** | `modules/ingress/`, `services/llunde-frontend/` | Caddy quadlet under edge + Caddyfile (both vhosts, LE, ops-endpoints blocked), frontend unit | ADR 005, 006, 009, 013 | closure builds; Caddyfile review |
| **D — Backups & observability** | `modules/backups/`, `modules/observability/` | restic module w/ per-service options + llunde-backend adoption (pg_dump hook, AOF, weekly), node_exporter + journald, tailnet-only exposure | ADR 011, 013 | closure builds; timer/unit review |
| **E — Runbook** | `docs/runbook.md` | Bare-server → serving procedure incl. bootstrap, install, verify, update, rollback, restore | all | Complete; exercised (and corrected) by the lead in step 2 |

## Step 2 — Serial go-live (lead only, runbook-driven)

| # | Task |
|---|---|
| 2.1 | `tofu apply` (import server, rename llunde-01, firewall); add `api.llunde.no` DNS (grey-cloud) |
| 2.2 | nixos-anywhere install (wipes the box); host key → sops recipients; commit real encrypted secrets; rebuild |
| 2.3 | Verify Tailscale, users, quadlets, Caddy TLS; run the full gate checklist from [README](README.md) — auth lifecycle, isolation, auto-update, restore, reboot |
| 2.4 | Fix-the-doc rule: every runbook deviation is a docs bug fixed on the spot |
| 2.5 | Owner review → gate closed. Post-gate Should: close port 22 after days of Tailscale use |
