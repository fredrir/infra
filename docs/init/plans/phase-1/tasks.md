# Phase 1 — task breakdown & delegation

## Step 0 — Foundation (lead agent, sequential)

| # | Task | Detail |
|---|---|---|
| 0.1 | Directory skeleton | Create the ADR-003 tree; `git mv` the existing tofu roots (`llunde/`→`tofu/llunde/`, `pyparser/`→`tofu/pyparser/`, `modules/hetzner-server/`→`tofu/modules/hetzner/`) so history survives |
| 0.2 | Flake skeleton | `flake.nix` (nixpkgs + sops-nix inputs, `nixosConfigurations.llunde-01`), `hosts/llunde-01/default.nix` importing `modules/profiles/server.nix` (stub), `disko.nix` (ext4, from research/stack-notes.md) — must *evaluate*, nothing more |
| 0.3 | Contracts | `mkQuadlet` signature (user, name, container/network sections → etc-file text) + `modules/` stub files with option declarations, so streams write against real interfaces |

**Done when**: `nix flake check` passes on the skeleton; committed.

## Step 1 — Parallel streams (sub-agents)

Disjoint paths; every stream leaves `nix flake check` / `tofu validate` green. No stream runs `apply` or touches a host.

| Stream | Owns | Builds | Follows | Done when |
|---|---|---|---|---|
| **A — Tofu** | `tofu/**` | llunde root rewrite (import-based server def, rename→llunde-01, firewall 22/80/443, S3 backend block); pyparser relocation untouched semantically; provider/lock regeneration with `tofu init`; `modules/{hetzner,cloudflare,aws}` shells | ADR 002, 003 | `tofu validate` both roots; pyparser `plan` = No changes; llunde `plan` = expected set, unapplied |
| **B — Nix modules** | `modules/**`, `services/**`, `hosts/**` | `mkQuadlet` implementation to skeleton-unit level (dummy unit rendering into the users/<uid> path), profiles/server.nix with base opts (ssh, firewall, sysctl, linger), stub option sets for users/ingress/data/backups/observability/tailscale/secrets, service folder stubs | ADR 003, 004, 005 | `nixos-rebuild build --flake .#llunde-01` produces a closure |
| **C — CI & Renovate** | `.github/**`, `renovate.json` | Reusable `build-image.yml` (workflow_call: containerfile, context, image, doppler build-args), repo-CI workflow (flake check + tofu validate/plan), Renovate config | ADR 010 | Workflow YAML validates; repo CI green on the PR |
| **D — Docs reset** | root `README.md`, `docs/` (excluding this plan tree) | Root README rewritten for the target world; DOPPLER/CLOUDFLARE content relocated + purged of old-plan references; `old.deploy-*.yml` deleted | ADR 014, research | grep gate (README exit #4) clean |

Cross-repo task (lead or stream D, coordinated): amend llunde-backend `docs/base/phase-3` per [ADR 014](../../../decisions/014-scope-boundaries.md).

## Step 2 — Integration & gate (lead agent)

| # | Task |
|---|---|
| 2.1 | Merge streams; run the full exit-criteria list from [README](README.md) |
| 2.2 | S3 state initialization for the llunde root (the only remote write this phase) |
| 2.3 | Owner review of layout + ADR fidelity → gate closed |
