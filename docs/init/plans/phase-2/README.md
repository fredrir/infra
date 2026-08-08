# Phase 2 — llunde-01 goes live

Wipe `llunde-cpx22`, install NixOS declaratively, and bring the new llunde world up on it: Caddy ingress, backend (with Postgres + Valkey), frontend, backups, Tailscale, minimal observability. Ends with the runbook proven from a bare box and the old `/opt/llunde` world gone by construction. **This phase destroys the old llunde stack and openclaw — both accepted by the owner ([ADR 001](../../../decisions/001-nixos-declarative-host.md), [014](../../../decisions/014-scope-boundaries.md)). Brief downtime on llunde.no is accepted.**

**Reads first**: every ADR ([index](../../../decisions/README.md)), both research docs. Phase 1 gate must be closed.

## In scope

- **Tofu apply** (first act): llunde root — import the existing server, rename → `llunde-01`, firewall **22/80/443 (the current firewall is SSH-only — the old README's "public nginx" claim was stale; current ingress is a Cloudflare Tunnel, see [research/current-state.md](../../../research/current-state.md))**. DNS cutover therefore includes: add `api.llunde.no` A/AAAA, **repoint `llunde.no` from the tunnel CNAME to direct A/AAAA (grey-cloud)**, and retire the tunnel after cutover (manual Cloudflare, documented — zone-into-tofu is a Should that may land here or later).
- **Install**: nixos-anywhere (kexec) with Disko ext4; `--extra-files` injects the SSH host key so sops-nix decrypts from first boot ([ADR 007](../../../decisions/007-sops-nix-bootstrap.md)).
- **sops-nix**: age recipients from the host key; exactly three secrets committed encrypted — Doppler service token, Tailscale auth key, restic password.
- **Host modules for real**: profiles/server.nix (SSH keys-only, host firewall, unattended upgrades policy, sysctl `ip_unprivileged_port_start=80`), tailscale (auto-join via auth key; **port 22 stays open as break-glass until the gate proves Tailscale**, closure is a post-gate Should), users module (llunde-backend, llunde-frontend, edge — subuid/subgid, linger), observability (node_exporter + journald, tailnet-only exposure per [ADR 013](../../../decisions/013-observability-host-scope.md)).
- **mkQuadlet full implementation** incl. per-user `podman-auto-update.timer` and the daemon-reload-on-switch hook ([ADR 004](../../../decisions/004-quadlet-own-abstraction.md) wrinkle).
- **Services** ([ADR 005](../../../decisions/005-per-service-users-isolation.md), [006](../../../decisions/006-caddy-ingress.md)):
  - `services/llunde-backend/`: postgres + valkey (appendonly) + backend quadlets on a private per-user network; API on `127.0.0.1:8080`; resource caps declared (JVM `-Xmx`, `shared_buffers`, `maxmemory`); Doppler wrapper entrypoint (host token from sops-nix → env for the container, mirroring backend ADR 009's zero-coupling rule); prod JVM flags from the backend's docs (`-Dlogback.configurationFile=logback-prod.xml`, `--enable-native-access=ALL-UNNAMED`).
  - `services/llunde-frontend/`: static-server container on `127.0.0.1:8081`.
  - `modules/ingress/` + edge user: Caddy quadlet, Caddyfile `llunde.no` → frontend, `api.llunde.no` → backend, Let's Encrypt; operational endpoints (`/metrics`, `/health`, `/ready`) blocked at Caddy (backend phase-3 finding, honored here).
- **Backups** ([ADR 011](../../../decisions/011-backups-restic-module.md)): restic module with per-service options; llunde-backend adopts it (pg_dump pre-hook + Valkey AOF, weekly); one restore proven at the gate.
- **Cross-repo (prerequisites, may run in parallel with authoring)**: llunde-backend gets its Containerfile + `build-image.yml` caller (its rescoped phase 3 — executed in that repo); llunde-frontend gets Containerfile + caller ([ADR 009](../../../decisions/009-frontend-as-container.md)). Both images must exist in GHCR before cutover.
- **Runbook** (`docs/runbook.md`): bare Hetzner server → serving, including secrets bootstrap, install, verify, update (auto-update semantics), rollback (NixOS generations + pinned image digests), restore-from-restic.

## Out of scope

pyparser (phase 3); Cloudflare orange-cloud (phase 4); GitOps CI-apply (phase 4); observability collection stack (phase 4); closing port 22 (post-gate Should, after Tailscale is proven in daily use).

## Exit criteria (gate)

1. The runbook executes top-to-bottom on the freshly wiped box without improvisation — every deviation found is fixed in the doc.
2. `https://api.llunde.no/ready` → 200 `{"database":true,"valkey":true}` through Caddy with a real certificate; `https://llunde.no` serves the frontend; `/metrics|/health|/ready` are NOT reachable publicly, but ARE over the tailnet.
3. The full backend auth lifecycle passes against prod via curl (register→login→me→sessions→logout-all; CSRF and problem+json behavior intact behind the proxy — real client IPs verified in audit logs, not proxy IPs).
4. Isolation proven: frontend user cannot reach Postgres/Valkey (no route exists); only Caddy binds public ports; each service survives `systemctl --user restart` under its own user.
5. Auto-update proven: push a trivial image change → box picks it up without SSH.
6. Backups proven: restic snapshot exists in S3 AND one restore of the Postgres dump is performed successfully.
7. Reboot test: full box reboot → everything returns without manual action.
8. `ssh` over Tailscale works (root); break-glass 22 documented with its closure procedure.
9. Owner review → gate closed. Old stack and openclaw are gone; nothing references them.
