# llunde-infra

Infrastructure for the llunde systems. One idea governs the repo:

> **Git describes desired state.** OpenTofu declares what machines exist; NixOS declares everything about them; Quadlet declares what runs on them; GHCR carries the app images; Doppler carries runtime secrets; sops-nix carries the bootstrap secrets that make the rest reachable.

Two Hetzner hosts, both NixOS, both fully declarative:

| Host | Runs |
|---|---|
| **llunde-01** | Caddy ingress, llunde backend + frontend, Postgres, Valkey |
| **llunde-parser** | the pyparser stack, the portfolio tenant slot, the observability collection stack |

**Zero public inbound ports, estate-wide.** Web traffic arrives through Cloudflare Tunnels the hosts dial outbound, SSH rides Tailscale, and no record in the `llunde.no` zone resolves to a host address ([ADR 015](docs/decisions/015-tailnet-only-management.md), [ADR 017](docs/decisions/017-tunnel-ingress-llunde.md)). Caddy still binds 80/443 behind a closed firewall as the break-glass path, renewing its certificates over DNS-01 so they stay warm ([docs/cloudflare.md](docs/cloudflare.md)).

**Deploys are pull.** A push to `main` that passes the CI gate fast-forwards the `deploy` ref; each host polls `deploy` and applies it itself — llunde-01 immediately as the canary, llunde-parser 30 minutes behind. No CI job SSHes a host, so no standing credential reaches the estate ([ADR 020](docs/decisions/020-gitops-pull-auto-apply.md)).

## Layout

| Path | What |
|---|---|
| `flake.nix` / `flake.lock` | The repo is a flake. `nixos-rebuild build --flake .#llunde-01`; `nix flake check` asserts the rendered-artifact goldens |
| `hosts/` | Per-host NixOS config — **diffs only**; everything shared comes from profiles |
| `modules/` | Shared NixOS modules: profiles, quadlet (our own `mkQuadlet`), users, tenants, ingress, data, backups, observability, tailscale, secrets, gitops-pull |
| `services/` | Per-service Nix modules, flat: `llunde-backend/`, `llunde-frontend/`, `pyparser/`, `observability/` |
| `images/caddy/` | The one image built here: Caddy with the Cloudflare DNS-01 provider compiled in |
| `tofu/` | OpenTofu — **one flat root** (both servers, the `llunde.no` zone, pyparser's S3/IAM) + shared `modules/` (hetzner, cloudflare) |
| `secrets/` | sops-encrypted bootstrap secrets ([secrets/README.md](secrets/README.md)) |
| `tests/golden/` | Rendered unit/config text the flake check compares against |
| `docs/` | [runbook](docs/runbook.md) · [decisions (ADRs)](docs/decisions/README.md) · [backlog](docs/backlog.md) · [doppler](docs/doppler.md) · [cloudflare](docs/cloudflare.md) · [pyparser](docs/pyparser/) · [research](docs/research/) and [init](docs/init/) (historical) |
| `.github/workflows/` | The gate (`check.yml`), the `deploy` promoter (`promote.yml`), the advisory `path-guard.yml`, the caddy image build, and the reusable `build-image.yml` service repos call |

## Where to start

- **How to operate it** — [docs/runbook.md](docs/runbook.md).
- **Why it is built this way** — [docs/decisions/](docs/decisions/README.md), 20 ADRs.
- **What is deliberately deferred** — [docs/backlog.md](docs/backlog.md).

## State management

One flat root, one state: `s3://llunde-pyparser-bucket/tofu-state/infra.tfstate`, encrypted, with lockfile-based locking. Never local, never committed (`.gitignore` enforces `*.tfstate`); the pre-merge per-project keys stay in the versioned bucket as rollback anchors ([ADR 002](docs/decisions/002-opentofu-s3-state.md)).

## Service repos

| Repo | Runs as | Infra home |
|---|---|---|
| [llunde-backend](https://github.com/fredrir/llunde-backend) | Quadlet container under its own user on llunde-01 | `services/llunde-backend/` |
| [llunde-frontend](https://github.com/fredrir/llunde-frontend) | Quadlet container under its own user on llunde-01 | `services/llunde-frontend/` |
| llunde-pyparser | Rootless Quadlet stack under `pyparser` on llunde-parser | `services/pyparser/`, [docs/pyparser/](docs/pyparser/) |

Service repos own their app and Containerfile and call this repo's reusable build workflow; this repo owns everything about running them ([ADR 014](docs/decisions/014-scope-boundaries.md)). `hansteen.dev` (portfolio) is a **tenant** instead: this repo declares its user slot and host dependencies and nothing inside it ([ADR 016](docs/decisions/016-tenant-slots-host-uniformity.md)).
