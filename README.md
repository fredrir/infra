# llunde-infra

Infrastructure for the llunde systems. One idea governs the repo:

> **Git describes desired state.** OpenTofu declares what machines exist; NixOS declares everything about them; Quadlet declares what runs on them; GHCR carries the app images; Doppler carries runtime secrets; sops-nix carries the three bootstrap secrets that make the rest reachable.

**Status: phase 1 (repo surgery) in progress — see [docs/init/plans/](docs/init/plans/phase-1/README.md).** Nothing here is applied to a live host until phase 2.

## Layout

| Path | What |
|---|---|
| `flake.nix` / `flake.lock` | The repo is a flake. `nixos-rebuild build --flake .#llunde-01` |
| `hosts/` | Per-host NixOS config — **diffs only**; everything shared comes from profiles |
| `modules/` | Shared NixOS modules: profiles, quadlet (our own `mkQuadlet`), users, ingress, data, backups, observability, tailscale, secrets |
| `services/` | Per-service Nix modules, flat, named like their repos: `llunde-backend/`, `llunde-frontend/`, `pyparser/` |
| `tofu/` | OpenTofu: `llunde/` and `pyparser/` root modules + shared `modules/` (hetzner, cloudflare, aws) |
| `docs/` | [The plan](docs/init/README.md) · [decisions (ADRs)](docs/decisions/README.md) · [research](docs/research/) · [doppler](docs/doppler.md) · [cloudflare](docs/cloudflare.md) |
| `.github/workflows/` | Repo CI + reusable workflows service repos call (`build-image.yml`) |

## Where to start

- **What's being built and in what order** — [docs/init/README.md](docs/init/README.md) (ownership map, MoSCoW, phases).
- **Why it's built that way** — [docs/decisions/](docs/decisions/README.md), 14 ADRs.
- **What's actually true right now** — [docs/research/current-state.md](docs/research/current-state.md).

## State management

OpenTofu state lives in S3 (`llunde-pyparser-bucket`), one state per root module — never local, never committed (`.gitignore` enforces `*.tfstate`). The pyparser root migrates its state in phase 3; until then it is deliberately untouched ([ADR 014](docs/decisions/014-scope-boundaries.md)).

## Service repos

| Repo | Runs as | Infra home |
|---|---|---|
| [llunde-backend](https://github.com/fredrir/llunde-backend) | Quadlet container under its own user | `services/llunde-backend/` |
| [llunde-frontend](https://github.com/fredrir/llunde-frontend) | Quadlet container under its own user | `services/llunde-frontend/` |
| pyparser | Docker Compose on its own host (until phase 3) | `services/pyparser/`, `tofu/pyparser/` |

Service repos own their app and Containerfile and call this repo's reusable build workflow; this repo owns everything about running them ([ADR 014](docs/decisions/014-scope-boundaries.md)).
