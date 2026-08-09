# llunde-infra — initialization plan

This directory is the complete plan for rebuilding llunde's infrastructure as a declarative, NixOS-based system. All decisions were settled with the owner in a design-interview session on 2026-08-09 and are recorded as ADRs. The governing idea:

> **Git describes desired state.** OpenTofu declares what machines exist; NixOS declares everything about them; Quadlet declares what runs on them; GHCR carries the app images; Doppler carries runtime secrets; sops-nix carries the three bootstrap secrets that make the rest reachable.

## Layout

| Path | What it holds |
|---|---|
| [`../decisions/`](../decisions/README.md) | 14 accepted ADRs — the *why* behind every structural choice |
| [`../research/`](../research/) | Facts the plan rests on: repo/infra audit, mechanics notes |
| [`plans/phase-1/`](plans/phase-1/README.md) | **Repo surgery** — full plan (README + tasks) |
| [`plans/phase-2/`](plans/phase-2/README.md) | **llunde-01 goes live** — full plan (README + tasks) |
| [`plans/phase-3/`](plans/phase-3/README.md) | **pyparser groundwork, one management plane** — full plan (README + tasks); rescoped after the phase-2 gate |
| [`plans/phase-3.5/`](plans/phase-3.5/README.md) | **The final push to full NixOS** (llunde-parser incl. pyparser + portfolio tenant) — **full plan** (README + contract + tasks) |
| [`plans/phase-4/`](plans/phase-4/README.md) | **Tunnel edge, observability collection, GitOps auto-apply** — full plan (README + tasks), re-evaluated post-3.5 |

## Ownership map

| Concern | Owner |
|---|---|
| Server / cloud firewall / DNS / S3 existence | OpenTofu (state in S3) — [ADR 002](../decisions/002-opentofu-s3-state.md) |
| Disk layout · OS install | Disko · nixos-anywhere — [ADR 001](../decisions/001-nixos-declarative-host.md) |
| OS config, users, SSH, host firewall, sysctls | NixOS modules — [ADR 001](../decisions/001-nixos-declarative-host.md), [003](../decisions/003-repo-shape.md) |
| App containers | Podman + Quadlet via own `mkQuadlet` Nix abstraction — [ADR 004](../decisions/004-quadlet-own-abstraction.md) |
| Service isolation | Per-service OS users, loopback-only cross-user — [ADR 005](../decisions/005-per-service-users-isolation.md) |
| Ingress / TLS | Caddy under `edge`, only 80/443 public — [ADR 006](../decisions/006-caddy-ingress.md) |
| Bootstrap secrets | sops-nix (3 secrets only) — [ADR 007](../decisions/007-sops-nix-bootstrap.md) |
| Runtime secrets | Doppler (host-side wrapper; app reads env) |
| Management access | Tailscale for humans and CI; port 22 retired — [ADR 008](../decisions/008-tailscale-management.md), [015](../decisions/015-tailnet-only-management.md) |
| Self-deploying tenants (portfolio) | Slot only (`modules/tenants/`): user + deps, tenant owns the rest — [ADR 016](../decisions/016-tenant-slots-host-uniformity.md) |
| CI | Reusable workflows here; pull-based deploy via auto-update — [ADR 010](../decisions/010-shared-ci-reusable-workflows.md) |
| Backups | restic → S3, per-service module options — [ADR 011](../decisions/011-backups-restic-module.md) |
| Edge (Cloudflare) | DNS-only now; proxy/WAF phase 4 — [ADR 012](../decisions/012-cloudflare-strategy.md) |
| Observability | node_exporter + journald now, tailnet-only; collection phase 4 — [ADR 013](../decisions/013-observability-host-scope.md) |
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
