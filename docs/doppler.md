# Doppler — runtime secrets

Doppler is the source of truth for **application runtime secrets**. It is deliberately *not* the bottom of the stack: a host needs a credential to reach Doppler in the first place, and that bootstrap layer is sops-nix — exactly three secrets (the Doppler service token, the Tailscale auth key, the restic repository password), encrypted in this repo to the host's SSH key ([ADR 007](decisions/007-sops-nix-bootstrap.md)). Everything else stays in Doppler.

## Projects and configs

| Project | `dev` | `prd` | `ci` |
|---|---|---|---|
| `llunde` | backend + frontend local dev | prod runtime env | build-time values for CI (e.g. `VITE_*` args) |
| `pyparser` | local dev | prod runtime env (host-rendered) | deploy SSH host/key for the legacy workflow |

- `dev`/`prd` hold app runtime secrets; `ci` holds only what CI genuinely needs.
- Tokens are read-only and scoped per config: a **host** gets a `prd` service token, **GitHub Actions** gets a `ci` token as a repo secret (`DOPPLER_TOKEN_LLUNDE_CI`, `DOPPLER_TOKEN_PYPARSER_CI`). A `prd` token cannot read `ci` secrets and vice versa.
- The llunde `ci` config still carries SSH-deploy secrets from the retired push-based workflow; delete them during phase 2 — the new CI never touches a host ([ADR 010](decisions/010-shared-ci-reusable-workflows.md)).

## How secrets flow

| Where | Mechanism |
|---|---|
| Local dev | `doppler run -- <cmd>` injects the `dev` config. Local defaults mean the llunde backend needs no secrets at all to run ([backend ADR 009](https://github.com/fredrir/llunde-backend)). |
| llunde prod (target, phase 2) | The host's `prd` service token arrives via sops-nix; the Quadlet entrypoint wrapper renders Doppler `prd` into container env at start. The app only ever reads plain env vars. |
| pyparser prod (unchanged until phase 3) | `deploy-remote.sh` renders `/opt/pyparser/secrets.env` from `prd` (`doppler secrets download --no-file --format docker`) before compose; the file is `chmod 600`, atomic-replaced. |
| CI builds | The reusable `build-image.yml` workflow can fetch build-time args (e.g. Vite public keys) from a `ci` config via `DOPPLER_TOKEN_*` repo secrets. CI never sees runtime secrets. |

## Token operations

```sh
# Host prd token (read-only)
doppler configs tokens create host --project <p> --config prd --plain --max-age 0
# CI ci token → store as GitHub repo secret
doppler configs tokens create github --project <p> --config ci --plain
```

Rotate by creating a new token and replacing it where it lives (sops-nix file for the llunde host; repo secret for CI; `doppler configure set token` on the pyparser host).

## Landmines

- **Never casually rotate** `TUNNEL_TOKEN` (pyparser `prd`) or the pyparser AWS access key — rotating drops the Cloudflare tunnel / breaks S3 backups. The llunde tunnel token becomes irrelevant when phase 2 retires that tunnel ([docs/cloudflare.md](cloudflare.md)).
- Doppler injects `DOPPLER_PROJECT/CONFIG/ENVIRONMENT` metadata into every render — harmless extra env vars.
- pyparser parity check (host file vs Doppler): see `docs/pyparser/PROD.md`.
