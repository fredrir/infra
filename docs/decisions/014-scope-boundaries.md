# ADR 014: Scope Boundaries

**Status**: Accepted · **Date**: 2026-08-09

## Context

A restructure this broad fails by scope creep more than by bad engineering. This ADR records the explicit boundary decisions from the design session — what belongs where, what is deferred, and what is deliberately not happening — so future sessions inherit the lines, not just the code.

## Decision

1. **Repo split**: service repos (llunde-backend, llunde-frontend, later pyparser's code) own their application, Containerfile, and a thin CI caller ([ADR 010](010-shared-ci-reusable-workflows.md)). **llunde-infra owns everything else**: OpenTofu ([ADR 002](002-opentofu-s3-state.md)), NixOS configuration ([ADR 001](001-nixos-declarative-host.md)), Quadlet units ([ADR 004](004-quadlet-own-abstraction.md)), ingress ([ADR 006](006-caddy-ingress.md)), DNS ([ADR 012](012-cloudflare-strategy.md)), backups ([ADR 011](011-backups-restic-module.md)), observability ([ADR 013](013-observability-host-scope.md)), and the runbook. This **rescopes the backend repo's phase 3**: its `docs/base/phase-3` shrinks to "Containerfile + image pipeline"; the Quadlet units, proxy contract, and runbook that plan carried move here. Amending those backend docs is a phase-1 task.
2. **pyparser**: zero functional change until phase 3, which runs **last** — it is the only system with real data (its Postgres, its AWS bucket) and absorbs no risk from the other systems' rebirth. Phase 3 covers: tofu relocation + state migration to S3 (steady-state `plan` proof), eventual NixOS adoption when the owner chooses to reinstall it, shared backup-module adoption, and the port-22-despite-tunnel audit ([ADR 008](008-tailscale-management.md)).
3. **openclaw**: wiped with the box ([ADR 001](001-nixos-declarative-host.md)) and **not redeployed by this repo** — it is declarative in its own repo and holds nothing important, per the owner. Its user slot in the per-service layout ([ADR 005](005-per-service-users-isolation.md)) stays reserved should a `services/openclaw/` module ever be wanted.
4. **Per-service `infra.yaml` contract files**: **Won't**, this round. The service contract (image name, loopback port, health path, env) is a documented convention in `services/<name>/` here — a cross-repo sync mechanism earns its complexity only when a fourth-plus service or real automation consumes it.
5. **Kubernetes**: explicit **non-goal** (owner horizon: possibly a year out). Nothing here blocks it — NixOS hosts can join k3s, images are already OCI, and the per-service contract translates — but no design concession is made for it now.
6. **Renovate**: adopted across all repos (flake inputs here; Gradle catalog and Actions in the service repos) — dependency currency is part of the estate, not per-repo initiative.

## Alternatives considered

- **Service repos own their own deploy config** — maximizes autonomy, but with one operator it just scatters the estate across repos and makes shared change N-repo work. Rejected.
- **Include pyparser in the main migration wave** — rejected: the only real data in the estate does not ride shotgun on a rewrite.
- **Build the infra.yaml contract now** — rejected as machinery without a consumer (round-1 Q7, reaffirmed after the NixOS pivot).

## Consequences

- The backend's phase-3 docs must be amended in phase 1, or two repos will claim ownership of the same Quadlet units.
- pyparser's phase-3 slot means its known oddities (open 22, ad-hoc backups) are *documented debt* until then — accepted consciously.
- Anyone (including future Claude sessions) proposing k8s, infra.yaml, or an early pyparser change should be pointed at this ADR and asked what changed.
