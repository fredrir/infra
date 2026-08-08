# Phase 1 — Repo surgery

Turn the transferred monorepo `infra/` folder into the target llunde-infra shape — flake at root, ADR-003 layout, OpenTofu, S3 state, shared CI, honest docs — **without touching any running infrastructure**. Everything in this phase is file movement, scaffolding, and evaluation; the only remote interaction is creating/migrating Terraform state in S3 and a read-only `plan` against pyparser.

**Reads first**: [ADR 001–004](../../../decisions/README.md), [010](../../../decisions/010-shared-ci-reusable-workflows.md), [014](../../../decisions/014-scope-boundaries.md), [research/current-state.md](../../../research/current-state.md).

## In scope

- **Layout restructure** per [ADR 003](../../../decisions/003-repo-shape.md): `flake.nix` at root; `hosts/llunde-01/` (skeleton: `default.nix` importing profiles, `disko.nix` single-disk ext4); `modules/` (profiles/server.nix stub, quadlet/, users/, ingress/, data/, backups/, observability/, tailscale/, secrets/ — stubs that evaluate); `services/{llunde-backend,llunde-frontend,pyparser}/` (folders, `default.nix` stubs); `tofu/{llunde,pyparser,modules/{hetzner,cloudflare,aws}}`; `docs/` (this tree).
- **OpenTofu migration**: all workflows/docs reference `tofu`; regenerate lock files with `tofu init`; **pyparser root untouched semantically** — only relocated, then `tofu plan` must be steady-state clean.
- **S3 state backend** ([ADR 002](../../../decisions/002-opentofu-s3-state.md)): backend block for the *llunde* root (fresh state — the old adopted-server state is retired with the old root); pyparser's migration is deliberately deferred to phase 3.
- **tofu/llunde rewrite, plan-only**: create-mode Hetzner resources (server import of the existing box, rename → `llunde-01`, firewall 22/80/443), **no apply this phase** — apply is phase 2's first act.
- **mkQuadlet skeleton** ([ADR 004](../../../decisions/004-quadlet-own-abstraction.md)): the function signature + generated-file wiring (`/etc/containers/systemd/users/<uid>/`), evaluating with a dummy unit; real service units are phase 2.
- **Shared CI** ([ADR 010](../../../decisions/010-shared-ci-reusable-workflows.md)): `.github/workflows/build-image.yml` reusable workflow (Containerfile path, image name, optional Doppler build-args); a CI workflow for this repo itself (`nix flake check` + `tofu validate` + `tofu plan` where credentials allow).
- **Docs reset** (owner requirement): delete `old.deploy-*.yml` (facts preserved in [research/current-state.md](../../../research/current-state.md)); rewrite root README to describe the target world; relocate DOPPLER/CLOUDFLARE docs under `docs/`, purging every old-internal-plan reference.
- **Renovate**: config for this repo (flake inputs, tofu providers, actions).
- **Cross-repo**: amend llunde-backend's `docs/base/phase-3` to its rescoped shape (Containerfile + CI caller only; quadlet/runbook/proxy pointers → here).

## Out of scope

Any `tofu apply`; any SSH to any box; nixos-anywhere; sops-nix key material (phase 2 — no host key exists yet); pyparser state migration (phase 3); frontend/backend repo Containerfiles (phase 2).

## Exit criteria (gate)

1. `nix flake check` passes; `nixos-rebuild build --flake .#llunde-01` produces a system closure (proves the skeleton is a real, buildable NixOS config — never applied).
2. `tofu -chdir=tofu/pyparser plan` → **"No changes."** (the do-no-harm proof); `tofu -chdir=tofu/llunde validate` clean and `plan` shows exactly the expected import/rename/firewall set, applied by no one.
3. llunde root state lives in S3; no `*.tfstate` tracked by git anywhere.
4. `git grep -i` for old-plan references, `old.deploy`, and `LINODE` returns nothing outside `docs/research/`.
5. Reusable `build-image.yml` is syntactically valid and callable (dry validation).
6. Backend repo's phase-3 docs amended and committed there.
7. Owner review → gate closed, phase 2 unblocked.
