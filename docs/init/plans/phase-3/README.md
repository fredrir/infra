# Phase 3 — pyparser, carefully, last (top-level plan)

pyparser is live production with real data (Postgres on-box + AWS S3) — it moves last, changes minimally, and every step carries a proof. Detailed task breakdown is deliberately deferred until phases 1–2 have landed and taught their lessons ([ADR 014](../../../decisions/014-scope-boundaries.md)).

## Intended scope (from the settled decisions)

- **State migration**: `tofu/pyparser` → S3 backend via `tofu init -migrate-state`; safety proof = steady-state `plan` shows **"No changes."** before and after ([ADR 002](../../../decisions/002-opentofu-s3-state.md)).
- **Security decision**: pyparser's public port 22 is *deliberate* firewall config (research corrected the earlier misconfiguration hypothesis — see [research/current-state.md](../../../research/current-state.md)). The phase-3 item is a decision, not a fix: keep 22 open, or close it and align pyparser's management path with [ADR 008](../../../decisions/008-tailscale-management.md) (Tailscale) / its existing tunnel.
- **NixOS adoption — when the owner chooses**: fold the pyparser host into `hosts/` + `services/pyparser/` via the same nixos-anywhere path, with a data-first plan: verified backups (restic) BEFORE any reinstall, restore rehearsal, and an explicit maintenance window. Until that decision, the host stays exactly as it is.
- **Backups adoption**: pyparser's existing backup arrangement reviewed and folded into `modules/backups/` per-service options ([ADR 011](../../../decisions/011-backups-restic-module.md)).
- **Tofu unnesting**: after pyparser is migrated, the per-project roots merge to flat `tofu/*` — accepting the recorded consequence that two states become one blast radius; `prevent_destroy` + Hetzner delete-protection stay on the pyparser server throughout ([ADR 002](../../../decisions/002-opentofu-s3-state.md)).

## Hard rules

1. Zero functional change to pyparser until this phase, and inside it: one change at a time, each with its own rollback note.
2. Backup verified by restore before anything host-mutating.
3. `parser.llunde.no` availability is the gate metric for every step.

## Gate (to be detailed later)

State in S3 with clean plans; firewall audited and corrected; backups proven; (if adopted) host declarative with the runbook exercised; tofu flat with pyparser protections intact.
