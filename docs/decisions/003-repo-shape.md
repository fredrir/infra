# ADR 003: Repository Shape — Flake at Root, Hosts as Diffs, Flat Service Modules

**Status**: Accepted · **Date**: 2026-08-09

## Context

Two candidate layouts were on the table: the flake nested under `nixos/` with OpenTofu roots at various levels, versus the flake at the repository root. Separately, an earlier round chose *nested* service directories (`services/llunde/frontend`) to mirror server grouping, and later floated per-service tofu directories (`/services/llunde-backend/` as tofu roots). The NixOS pivot ([ADR 001](001-nixos-declarative-host.md)) changed what "per-service configuration" even is: under NixOS it is a Nix module, and what remains for OpenTofu is per-host/project resources — per-service tofu directories would be near-empty shells. The owner also set a hard requirement: **hosts must contain as little duplication as possible**.

## Decision

```
llunde-infra/
├── flake.nix, flake.lock          # the repo IS a flake
├── hosts/
│   └── llunde-01/{default.nix, disko.nix}   # diffs only
├── modules/
│   ├── profiles/server.nix        # shared baseline every host imports
│   ├── quadlet/  users/  ingress/  data/  backups/  observability/  tailscale/  secrets/
├── services/                      # flat, per-service FOLDERS, names match service repos
│   ├── llunde-backend/default.nix
│   ├── llunde-frontend/default.nix
│   └── pyparser/                  # phase 3
├── tofu/
│   ├── llunde/  pyparser/  modules/{hetzner,cloudflare,aws}/
├── docs/{init,research,decisions}/
└── .github/workflows/             # reusable CI (ADR 010)
```

- **Flake at the root**: `nixos-rebuild --flake .#llunde-01` from anywhere; the flake is the repo's identity and every tool expects it there.
- **Hosts are diffs**: `hosts/llunde-01/default.nix` imports `modules/profiles/server.nix` plus the service modules it runs, and declares only what makes this machine itself (hostname, disko, service selection, host-specific caps).
- **Services are flat folders** (`services/llunde-backend/`), named identically to their source repositories. The earlier nesting instinct ("reflect the server split") is expressed where it belongs: in which host imports which services, not in directory depth.
- **OpenTofu lives contained in `tofu/`** with the root-module evolution described in [ADR 002](002-opentofu-s3-state.md).
- **Host naming is identity, not hardware**: `llunde-01`, not `llunde-cpx22` — the Hetzner server gets renamed to match. Type-in-name breaks the day the box is rescaled.

## Alternatives considered

- **Flake under `nixos/`, tofu at root** — inverts importance: tofu describes a handful of cloud resources; Nix describes everything else.
- **Nested `services/llunde/frontend`** — superseded; encodes a coupling the owner is explicitly removing.
- **Per-service tofu directories** — superseded by the NixOS pivot; services have no cloud resources of their own.

## Consequences

- Adding a service = one folder in `services/` + one import line in a host. Adding a host = one folder in `hosts/` importing the shared profile.
- Dedup is structural: anything two hosts share *must* move into `modules/`, or it gets written twice and flagged in review.
- `pyparser.nix`'s eventual home is already reserved; phase 3 fills it without reshaping anything.
