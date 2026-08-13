# Doppler — runtime secrets

Doppler is the source of truth for **application runtime secrets**. It is
deliberately not the bottom of the stack: a host needs a credential to reach
Doppler at all, and that bootstrap layer is sops-nix
([ADR 007](decisions/007-sops-nix-bootstrap.md),
[secrets/README.md](../secrets/README.md)).

## Projects and configs

| Project | `dev` | `prd` | `ci` |
|---|---|---|---|
| `llunde` | backend + frontend local dev | prod runtime env | build-time values (e.g. `VITE_*` args) |
| `pyparser` | local dev | prod runtime env (host-rendered) | build-time values |

`llunde` also has an `ops` config — laptop-only operator secrets:
`CLOUDFLARE_API_TOKEN`, read by tofu as an env var (`doppler secrets get
CLOUDFLARE_API_TOKEN --project llunde --config ops --plain`). It carries
`Zone:DNS:Edit` on llunde.no **plus account-wide Cloudflare Tunnel Read/Edit**,
which reaches every tunnel in the account — never a host or CI secret
([cloudflare.md](cloudflare.md)).

- Tokens are read-only and scoped per config: a **host** gets a `prd` service
  token, **GitHub Actions** gets a `ci` token as a repo secret
  (`DOPPLER_TOKEN_LLUNDE_CI`, `DOPPLER_TOKEN_PYPARSER_CI`). A `prd` token cannot
  read `ci` secrets and vice versa.
- No SSH-deploy or tailnet credential belongs in a `ci` config: CI never touches
  a host ([ADR 010](decisions/010-shared-ci-reusable-workflows.md),
  [ADR 020](decisions/020-gitops-pull-auto-apply.md)). Anything left there from
  the retired push-based workflow is dead and should be deleted.

## How secrets flow

| Where | Mechanism |
|---|---|
| Local dev | `doppler run -- <cmd>` injects `dev`. Local defaults mean the llunde backend needs no secrets at all to run ([backend ADR 009](https://github.com/fredrir/llunde-backend)). |
| llunde prod | The host's `prd` token arrives via sops-nix as `/run/secrets/doppler.env`; the backend quadlet consumes it and the app reads plain env vars. |
| pyparser prod | A root oneshot (`pyparser-secrets-render`) renders `pyparser/prd` into `/run/pyparser/secrets.env` (0640, group `pyparser`), ordered before the pyparser user manager; every stack unit consumes it via `EnvironmentFile=`. |
| CI builds | `build-image.yml` can fetch build-time args (e.g. Vite public keys) from a `ci` config via `DOPPLER_TOKEN_*` repo secrets. CI never sees runtime secrets. |

## Token operations

```sh
# Host prd token (read-only)
doppler configs tokens create host --project <p> --config prd --plain --max-age 0
# CI ci token → store as GitHub repo secret
doppler configs tokens create github --project <p> --config ci --plain
```

Rotate by creating a new token and replacing it where it lives: the sops file
for a host (`secrets/doppler.yaml` for llunde-01, `secrets/pyparser-doppler.yaml`
for llunde-parser), the repo secret for CI.

## Landmines

- **Never casually rotate** `TUNNEL_TOKEN` (pyparser `prd`) or the pyparser AWS
  access key — rotating drops the Cloudflare tunnel / breaks S3 backups. The
  llunde tunnel's token is not here at all: it is a sops secret on llunde-01
  (`secrets/llunde-tunnel.yaml`), under the same rule
  ([cloudflare.md](cloudflare.md)).
- Doppler injects `DOPPLER_PROJECT/CONFIG/ENVIRONMENT` into every render —
  harmless extra env vars.
- pyparser parity check (host file vs Doppler): [pyparser/PROD.md](pyparser/PROD.md).
