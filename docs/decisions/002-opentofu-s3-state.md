# ADR 002: OpenTofu with Remote S3 State

**Status**: Accepted · **Date**: 2026-08-09

## Context

The transferred repo uses Terraform with two root modules (`llunde/`, `pyparser/`) sharing a `hetzner-server` module. State is **local**, backed up by manually running `aws s3 cp` after applies ([../research/current-state.md](../research/current-state.md)). That is the most fragile arrangement in the repo: a lost laptop or a forgotten copy leaves live infrastructure unmanageable. Separately, the owner wants off the Terraform license onto OpenTofu.

## Decision

- **Terraform → OpenTofu.** Drop-in and state-compatible; the migration is renaming the CLI in docs/recipes and re-running `tofu init`. HCL sources are unchanged.
- **Remote state in the existing AWS S3 bucket**, one state per root module, configured via an `s3` backend block and migrated with `tofu init -migrate-state`. The manual copy ritual dies.
- **Roots stay split for now**: `tofu/llunde/` is rewritten in create-mode for the new world (phase 2); `tofu/pyparser/` is relocated but functionally untouched until phase 3, with a steady-state `tofu plan` ("No changes") as the proof of harmlessness.
- **After phase 3 the roots unnest and merge to a flat `tofu/*`** (the owner's explicit end-state): one root covering servers, networking, firewall, DNS, storage across all llunde hosts.

## Alternatives considered

- **Stay on Terraform** — no functional gain over OpenTofu and a license the owner wants to leave.
- **Keep local state with disciplined backups** — the discipline is the failure mode.
- **Hetzner Object Storage as the backend** — S3-compatible and viable; deferred as a *Could* since the AWS bucket exists and works today. Consolidating off AWS is a later, isolated change.
- **Merge all roots immediately** — rejected: pyparser is the only system with real data (Postgres + AWS S3/IAM) and must not absorb risk from this refactor until everything else is proven.

## Consequences

- Applies become laptop-independent and crash-safe; state locking comes with the backend.
- The eventual root merge (post-phase-3) **collapses two blast radii into one**: a mistake in shared code can then touch pyparser. Mitigation is explicit and carried through the merge: pyparser's `prevent_destroy` lifecycle rules and Hetzner `delete_protection` stay on, and the merge itself is done as a state-move operation with plan-diff review, not a re-import.
- The old `llunde/` root's adoption pattern (`ignore_changes` on a pet server) is deleted, not migrated — the new root manages the machine for real ([ADR 001](001-nixos-declarative-host.md)).
- Related: repo layout for `tofu/` in [ADR 003](003-repo-shape.md).
