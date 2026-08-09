# Phase 3.5 — the final push to full NixOS (top-level plan)

`llunde-parser` is the last imperative machine. This phase wipes and reinstalls it as NixOS via the same nixos-anywhere path that built `llunde-01`, carrying **two live productions** across: pyparser (today: rootful docker-compose under `leploy`) and portfolio (today: self-managed rootless podman quadlets under `portfolio`). Owner-classified must-have; detailed planning starts only after the [phase-3 gate](../phase-3/README.md) closes — phase 3 deliberately front-loads everything that de-risks this one (state in S3, CI restored, tailnet-only management, zone in tofu).

The governing principle, owner-stated: **in the end there should be little to no difference between the boxes and services — NixOS on both, shared abstractions, configs, tunnels, and CI; only service-specific configuration differs per service** ([ADR 016](../../../decisions/016-tenant-slots-host-uniformity.md)).

## Intended scope

- **Host**: `hosts/` gains the second machine — same server profile, disko, sops-nix, tailscale, and mkQuadlet modules as `llunde-01`; only service wiring differs. (Naming — keep `llunde-parser` vs. rename into the `llunde-NN` scheme — is an owner decision at planning time, [ADR 003](../../../decisions/003-repo-shape.md).)
- **pyparser becomes a service**: `services/pyparser/` on the shared quadlet abstraction — per-user rootless podman, the compose stack's 8 services re-expressed as units. Known frictions, mapped in [research/phase-3-mapping.md](../../../research/phase-3-mapping.md): Dozzle's docker-socket proxy (podman socket story), containers running as root internally, `sleep 86400` backup loops becoming systemd timers (an upgrade), compose-only knobs (`mem_limit`, `shm_size`, `stop_grace_period`) mapping to quadlet/systemd equivalents, `pyparser-sync`'s `docker inspect` SSH tunnel.
- **Deploy model converges** ([ADR 010](../../../decisions/010-shared-ci-reusable-workflows.md)): alembic moves to service startup (or a oneshot migrate unit ordered before the app units — the change lands in the llunde-pyparser repo), worker drain semantics carry over as stop timeouts, pre-migrate `pg_dump` becomes a restic-style preHook — after which pyparser deploys exactly like llunde-backend: push-to-main → shared `build-image.yml` → GHCR → the box pulls. The phase-3 CI-SSH workflow is scaffolding and is retired here.
- **portfolio becomes a declared tenant slot** ([ADR 016](../../../decisions/016-tenant-slots-host-uniformity.md)): `modules/tenants/` provides what its Ubuntu `bootstrap.sh` does imperatively — user, subuid/subgid range, linger, podman, and its documented host binaries (cosign, doppler, python3) — and nothing more. portfolio keeps deploying itself into `~/.config/containers/systemd/`; llunde-infra never references its internals.
- **Backups**: pyparser's nightly dump + S3 ship folds into `modules/backups/` per-service options ([ADR 011](../../../decisions/011-backups-restic-module.md)); portfolio keeps its own (WAL-shipping PITR — tenant-owned by definition).
- **Data-first migration**: verified fresh backups **and rehearsed restores for both tenants** before the wipe — pyparser's Postgres + named volumes, portfolio's Postgres/WAL — plus preserved tunnel tokens (neither tunnel is recreated), an explicit maintenance window, and `parser.llunde.no` + `hansteen.dev` as the availability gates throughout.

## Hard rules

1. No reinstall before both tenants' restores are rehearsed end-to-end on a scratch target.
2. Tunnel tokens and Doppler tokens survive the wipe (sops-nix on the new host; portfolio's via its own flow) — neither tunnel is rotated.
3. One tenant cut over at a time; the gate metric pair holds at every step.

## Gate (to be detailed at planning time)

Both sites serving from NixOS with the runbook exercised end-to-end; pyparser on pull-based deploys via the shared CI; portfolio redeploying itself onto the declared slot with zero llunde-infra involvement; restores proven from post-migration backups; no imperative machine left.
