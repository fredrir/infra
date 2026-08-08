# ADR 011: Backups via Restic and a Shared NixOS Module

**Status**: Accepted · **Date**: 2026-08-09

## Context

The backend's original phase-3 plan had only a "backup note"; the first grilling round settled backups as a Should with a plain pg_dump timer. The NixOS pivot ([ADR 001](001-nixos-declarative-host.md)) and the owner's preferences sharpened that: restic as the engine, and — the owner's explicit design ask — a **shared backup module with per-service configuration**, so each service declares *what* is backed up and *how often* rather than owning bespoke scripts. pyparser reportedly already has some backup arrangement; it must not be touched until phase 3 ([ADR 014](014-scope-boundaries.md)).

## Decision

**Restic to the existing S3 bucket, wrapped in `modules/backups/` — a NixOS module exposing per-service options.**

- The module declares systemd services + timers (systemd is intentional architecture here, not an accident), generated per enabled service.
- Per-service options cover: paths and/or dump commands, schedule, retention. llunde-backend enables it with a `pg_dump` pre-hook (consistent snapshot, not raw datadir files) and the Valkey appendonly file; cadence starts **weekly** per the owner — tightened to daily-or-better the moment real users exist, by changing one option.
- The restic repository password is a bootstrap secret in sops-nix ([ADR 007](007-sops-nix-bootstrap.md)); S3 credentials likewise, scoped to the backup prefix.
- The **restore procedure lives in the runbook**, and the phase-2 gate requires one restore actually performed — an unrestored backup is a hope, not a backup.
- pyparser's existing backups stay untouched until phase 3, then migrate into this module with pyparser-specific options (its Postgres + whatever its current arrangement covers).

## Alternatives considered

- **Plain pg_dump-to-S3 timer** (round-1 design) — works, but no deduplication, no retention policy machinery, no encryption story, and every new service re-invents the script. Superseded by restic.
- **Hetzner snapshots** — whole-box images; coarse, unversioned per-service, and restores are all-or-nothing. Fine as belt (Could), not as the mechanism.
- **borgbackup** — comparable to restic; restic wins on native S3 support (no SSH target needed) and the owner named it.
- **Managed Postgres with built-in PITR** — out of scope; the DB stays on-box by the backend's ADR decisions.

## Consequences

- "Backup scheduling is declarative" holds: a service's backup posture is readable in its Nix module, and drift is impossible.
- Weekly cadence means up to a week of data loss until tightened — accepted explicitly by the owner for the zero-users phase; the tightening is a one-line change and is called out in the phase-2 gate notes.
- Restic's repo format adds a required password — losing it loses the backups; it lives in sops-nix and the owner's password manager both.
- The S3 bucket gains a lifecycle consideration (restic prune schedule) owned by the same module.
