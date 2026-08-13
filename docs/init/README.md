# llunde-infra — the build plan (archive)

> **✅ Every phase in this directory is complete.** The estate it describes was
> finished on 2026-08-13. **Nothing here is live guidance** — it is the record of
> how the system was built and in what order.
>
> For what is true now: the [repo README](../../README.md) for shape,
> [`docs/decisions/`](../decisions/README.md) for why, and
> [`docs/runbook.md`](../runbook.md) for how to operate it.

This directory is the plan for rebuilding llunde's infrastructure as a declarative, NixOS-based system. All decisions were settled with the owner in a design-interview session on 2026-08-09 and are recorded as ADRs. The governing idea, which survived intact:

> **Git describes desired state.** OpenTofu declares what machines exist; NixOS declares everything about them; Quadlet declares what runs on them; GHCR carries the app images; Doppler carries runtime secrets; sops-nix carries the bootstrap secrets that make the rest reachable.

## Layout

| Path | What it held | Outcome |
|---|---|---|
| [`../decisions/`](../decisions/README.md) | The *why* behind every structural choice | 20 ADRs, still live |
| [`../research/`](../research/) | Facts the plan rested on: repo/infra audit, mechanics notes | Historical snapshots |
| [`plans/phase-1/`](plans/phase-1/README.md) | **Repo surgery** | Gate closed 2026-08-09 |
| [`plans/phase-2/`](plans/phase-2/README.md) | **llunde-01 goes live** | Gate closed 2026-08-09 |
| [`plans/phase-3/`](plans/phase-3/README.md) | **pyparser groundwork, one management plane** — rescoped after the phase-2 gate | Gate closed 2026-08-09 |
| [`plans/phase-3.5/`](plans/phase-3.5/README.md) | **The final push to full NixOS** — llunde-parser, pyparser, portfolio tenant | Gate closed; the estate reached 100% NixOS |
| [`plans/phase-4/`](plans/phase-4/README.md) | **Tunnel edge, observability collection, GitOps auto-apply** | Gate closed 2026-08-13 — see its [Executed record](plans/phase-4/tasks.md) |

Everything below is the plan **as written on 2026-08-09**. Several items were
overtaken by later decisions — where that happened, the Owner column names the
ADR that governs today alongside the one that was planned.

## Ownership map

| Concern | Owner |
|---|---|
| Server / cloud firewall / DNS / S3 existence | OpenTofu (state in S3) — [ADR 002](../decisions/002-opentofu-s3-state.md) |
| Disk layout · OS install | Disko · nixos-anywhere — [ADR 001](../decisions/001-nixos-declarative-host.md) |
| OS config, users, SSH, host firewall, sysctls | NixOS modules — [ADR 001](../decisions/001-nixos-declarative-host.md), [003](../decisions/003-repo-shape.md) |
| App containers | Podman + Quadlet via own `mkQuadlet` Nix abstraction — [ADR 004](../decisions/004-quadlet-own-abstraction.md) |
| Service isolation | Per-service OS users, loopback-only cross-user — [ADR 005](../decisions/005-per-service-users-isolation.md) |
| Ingress / TLS | Caddy under `edge` — [ADR 006](../decisions/006-caddy-ingress.md); the public 80/443 path it assumed became tunnel ingress with **zero public inbound**, [ADR 017](../decisions/017-tunnel-ingress-llunde.md) |
| Bootstrap secrets | sops-nix — [ADR 007](../decisions/007-sops-nix-bootstrap.md). Planned as exactly three; the set grew as services landed, see `secrets/` |
| Runtime secrets | Doppler (host-side wrapper; app reads env) |
| Management access | Tailscale for humans and CI; port 22 retired — [ADR 008](../decisions/008-tailscale-management.md), [015](../decisions/015-tailnet-only-management.md) |
| Self-deploying tenants (portfolio) | Slot only (`modules/tenants/`): user + deps, tenant owns the rest — [ADR 016](../decisions/016-tenant-slots-host-uniformity.md) |
| CI | Reusable workflows here; pull-based deploy via auto-update — [ADR 010](../decisions/010-shared-ci-reusable-workflows.md); host config applies became pull too, [ADR 020](../decisions/020-gitops-pull-auto-apply.md) |
| Backups | restic → S3, per-service module options — [ADR 011](../decisions/011-backups-restic-module.md) |
| Edge (Cloudflare) | Zone into tofu — [ADR 012](../decisions/012-cloudflare-strategy.md); its planned orange-cloud proxy became tunnel ingress, [ADR 017](../decisions/017-tunnel-ingress-llunde.md) |
| Observability | Exporters + journald, tailnet-only — [ADR 013](../decisions/013-observability-host-scope.md); the deferred collection stack landed on llunde-parser, [ADR 018](../decisions/018-observability-collection.md) |
| Scope boundaries (repos, pyparser, openclaw, k8s) | [ADR 014](../decisions/014-scope-boundaries.md) |

## MoSCoW

| | |
|---|---|
| **Must** | NixOS via nixos-anywhere + Disko; OpenTofu + S3 state; flake-root repo shape; `mkQuadlet`; per-user isolation + loopback wiring; sops-nix bootstrap; Caddy ingress; Tailscale; reusable build-image CI; backend + frontend live on `llunde-01`; docs reset; runbook |
| **Should** | restic backup module (weekly to start); Cloudflare zone into tofu; Renovate everywhere; observability module (minimal scope); close port 22 after Tailscale proven |
| **Could** | `deploy-now` CI poke; colmena/deploy-rs; Hetzner Object Storage for state; further SSH hardening |
| **Won't (this round)** | per-service `infra.yaml` contract files; Kubernetes; CF proxy/WAF (phase 4); GitOps CI-apply (phase 4); any pyparser change (until phase 3); full observability collection stack |

## Execution model

Same model that built the backend: each phase runs as its own working session ending in a **gate** the owner reviews before the next phase starts.

1. **Foundation step (lead, sequential)** — the serial part: skeletons, contracts, anything later streams compile/evaluate against.
2. **Parallel streams (sub-agents)** — independent workstreams cut along directory boundaries so agents never touch the same files. Each stream brief names the ADRs it must follow and its done-criteria; sub-agents never make new architectural decisions — anything uncovered goes back to the lead.
3. **Integration + gate (lead)** — the lead merges, applies (all host mutations are lead-only), runs the gate checklist, and presents results.

Infra-specific rule: **declarative files can be authored in parallel; the live host is only ever touched serially by the lead.** `tofu plan` / `nix flake check` are the compile-equivalents every stream must leave green.

## Cross-repo touchpoints

- **llunde-backend**: its `docs/base/phase-3` shrinks to Containerfile + CI caller (amended in phase 1); quadlet/runbook/proxy live here ([ADR 014](../decisions/014-scope-boundaries.md)).
- **llunde-frontend** (freshly split): needs a Containerfile + CI caller in phase 2 ([ADR 009](../decisions/009-frontend-as-container.md)).
- **openclaw**: wiped with the box, not redeployed by this repo; user slot reserved.
