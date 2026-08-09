# ADR 016: Tenant Slots, and Uniform Hosts as the End State

**Status**: Accepted · **Date**: 2026-08-09 (post-phase-2 planning round)

## Context

portfolio (hansteen.dev) runs on `llunde-parser` — live since 2026-07, discovered fully-formed during the phase-3 mapping ([research/phase-3-mapping.md](../research/phase-3-mapping.md)). The owner had already rejected making portfolio a first-class llunde-infra service: neither repo should need to know the other's internals. The mapping showed portfolio independently arrived at the same architecture this repo builds — dedicated user, subuids, linger, rootless podman quadlets in its own `~/.config/containers/systemd/`, its own tunnel, its own secrets and backups — bootstrapped imperatively by an Ubuntu-specific script.

Separately, the owner set the end-state bar for the whole estate: *little to no difference between the boxes and the services — NixOS on both, shared abstractions, configs, tunnels, and CI; only service-specific configuration differs.*

## Decision

Two commitments:

1. **Tenant slots.** For workloads that deploy themselves (portfolio; anything similar later), llunde-infra declares only the *slot*: OS user, uid + subuid/subgid range, linger, podman, and the host binaries the tenant documents needing (for portfolio: cosign, doppler, python3) — in `modules/tenants/`, one entry per tenant. Everything inside `$HOME` — quadlets, env files, deploy scripts, tunnel token — is the tenant's own, deployed by the tenant's own pipeline. llunde-infra never references tenant internals; tenants never reference llunde-infra. The contract is the slot definition, nothing more.
2. **Host uniformity as the end state.** Both machines run NixOS from this flake with the same profile, secrets, tailscale, quadlet, and backup modules; a host file composes shared modules plus its service/tenant list and contributes no bespoke logic. [Phase 3.5](../init/plans/phase-3.5/README.md) is the execution of this decision for `llunde-parser`; it is owner-classified must-have.

## Alternatives considered

- **portfolio as a llunde-infra service** (`services/portfolio/`) — rejected earlier by the owner and doubly wrong now: the mapping shows a complete, live deploy system (blue/green, cosign verification, PITR) that this repo would have to mirror, badly, to add nothing.
- **portfolio stays entirely self-bootstrapped on NixOS too** — its `bootstrap.sh` is apt/adduser imperative and cannot run on NixOS; someone must declare the user and packages. The slot is the minimal such someone.
- **Boxes stay heterogeneous** (NixOS for llunde-01, Ubuntu-forever for llunde-parser) — rejected by the owner: must-have NixOS on both. Heterogeneity is precisely the two-patterns-at-once state [ADR 008](008-tailscale-management.md) argued against for access paths, generalized to whole machines.

## Consequences

- Phase 3.5 must rehearse **two** tenants' restores before any wipe — the price of co-tenancy on one box, paid once.
- The slot module is a real interface: portfolio's documented host needs (subuid range `200000-265535`, cosign, doppler, python3) become declared inputs; if a tenant's needs change, the change arrives as a slot PR here, still content-free about internals.
- A future third tenant is a data point in `modules/tenants/`, not a design event.
- `hansteen.dev` (zone, tunnel, Access) remains permanently outside this repo's Cloudflare scope — [ADR 012](012-cloudflare-strategy.md)'s zone work covers `llunde.no` only.
