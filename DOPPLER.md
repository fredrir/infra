# Doppler — secrets management for pyparser + llunde

Doppler is the single source of truth for all app secrets, replacing the old
hand-managed `.env` / `secrets.env` files. Two projects, each with three configs.

```
project: pyparser                          project: llunde
  dev  local dev secrets (pyparser/.env)     dev  backend + frontend dev secrets
  prd  prod runtime (host renders)           prd  prod runtime (host renders)
  ci   PYPARSER_HETZNER_SSH_KEY, _HOST        ci   LINODE_SSH_KEY/HOST/USER,
                                                   VITE_TURNSTILE_SITE_KEY (prod)
```

- **dev / prd** hold **app runtime secrets**. **ci** holds **deploy-only** secrets.
- **Tokens** (read-only service tokens): the **host** gets a `prd` token; **GitHub
  Actions** gets a `ci` token (as a repo secret). A token can read every secret in
  its config, so the host token cannot read the deploy SSH key (it's in `ci`).

## How secrets flow

| Where | Mechanism |
|---|---|
| Local dev | `doppler run -- <cmd>` injects the `dev` config as env vars. `doppler.yaml` in `pyparser/`, `backend/`, `frontend/` pins the dev config. |
| pyparser prod | `deploy-remote.sh` renders `/opt/pyparser/secrets.env` from `prd` (`doppler secrets download --no-file --format docker`) before compose. Compose reads it via `env_file:`. |
| llunde prod | The deploy step's SSH command renders `/opt/llunde/.env` from `prd`, then `docker compose up`. Compose auto-loads `.env` for `${VAR}` interpolation. |
| CI (both) | `dopplerhq/cli-action@v3` + `DOPPLER_TOKEN` (repo secret) fetches deploy creds from `ci` with `doppler secrets get --plain`. CI never sees app runtime secrets. |

The two hosts reach `api.doppler.com` over outbound HTTPS (already open — cloudflared
dials out); the SSH-only inbound firewall is unaffected.

## Local development

```sh
doppler login                 # once, browser auth
cd pyparser && doppler setup  # binds this dir to pyparser/dev (reads doppler.yaml)
doppler run -- pyparser-worker
# backend:  cd backend  && doppler setup && doppler run -- pnpm dev
# frontend: cd frontend && doppler setup && doppler run -- pnpm dev
```

`pyparser/.envrc` keeps the machine-path exports (CUDA/HF/uv/pip) and venv
activation; it no longer relies on `.env` for secrets.

## Operating in production

Deploys are unchanged for the operator — push to `main` (or run the workflow
manually). On deploy the host re-renders its secret file from Doppler `prd`, so a
secret change = update it in Doppler, then redeploy (or re-run the render on the
host). To change a secret without a code deploy:

```sh
doppler secrets set SOME_KEY="new-value" -p llunde -c prd
ssh deploy "cd /opt/llunde && doppler secrets download --no-file --format docker > .env.new && mv .env.new .env && \
  docker compose -f docker-compose.prod.yml up -d --wait"
# pyparser: ssh leploy, /opt/pyparser/secrets.env, then re-run the affected services
```

## Host bootstrap (one-time, per host — done during migration)

```sh
# Install the CLI (as root):  pyparser=letzner, llunde=hetzner
ssh letzner 'curl -sLf --retry 3 https://cli.doppler.com/install.sh | sh'

# Configure the read-only prd token for the DEPLOY user, scoped to the app dir:
#   pyparser deploy user = leploy @ /opt/pyparser
#   llunde   deploy user = deploy @ /opt/llunde
ssh leploy 'doppler configure set token <PRD_SERVICE_TOKEN> --scope /opt/pyparser'
ssh deploy 'doppler configure set token <PRD_SERVICE_TOKEN> --scope /opt/llunde'
```

`doppler` must be on `PATH` for a non-interactive SSH shell (the install script
puts it in `/usr/local/bin`, which is). Verify: `ssh leploy 'doppler --version'`.

## Tokens & GitHub secrets

- **Host prd tokens** — `doppler configs tokens create host --project <p> --config prd
  --plain --max-age 0` (read-only), placed on the host as above. Rotate by creating a
  new token and re-running `doppler configure set token`.
- **CI ci tokens** — `doppler configs tokens create github --project <p> --config ci
  --plain`, stored as GitHub repo secrets:
  - `DOPPLER_TOKEN_PYPARSER_CI`
  - `DOPPLER_TOKEN_LLUNDE_CI`
- After a green cutover, the **old** GitHub secrets are unused and can be deleted:
  `PYPARSER_HETZNER_SSH_KEY`, `PYPARSER_HETZNER_HOST`, `LINODE_SSH_KEY`,
  `LINODE_HOST`, `LINODE_USER`, `VITE_TURNSTILE_SITE_KEY`. Keep `GITHUB_TOKEN`.

## Verification

```sh
# Parity: does Doppler prd match the live host file? (expect only DOPPLER_* extras)
ssh leploy 'doppler secrets download --no-file --format docker > /tmp/c.env; \
  diff <(grep -vE "^DOPPLER_" /tmp/c.env | sort) <(sort /opt/pyparser/secrets.env); rm /tmp/c.env'
# llunde: same on `ssh deploy` against /opt/llunde/.env (IMAGE_TAG is expected to
# differ — it's supplied by CI, not Doppler).

# Local: doppler run -- python -c "from pyparser.config import get_settings; print(get_settings().env)"
```

Post-deploy smoke: `https://parser.llunde.no/healthz` and `https://llunde.no` load —
the Cloudflare tunnels staying up confirms `TUNNEL_TOKEN` rendered correctly.

## Landmines

- **Never rotate** `TUNNEL_TOKEN` (both apps) or the pyparser AWS access key —
  rotating drops the Cloudflare tunnel / breaks S3 backups. They were imported
  verbatim; leave them.
- Keep `IMAGE_TAG` **out** of Doppler `prd` (CI supplies it on the compose command
  line; a Doppler value would override the deployed tag).
- Doppler injects `DOPPLER_PROJECT/CONFIG/ENVIRONMENT` metadata into every render.
  They are harmless extra env vars (ignored by both apps); the parity `diff` filters
  them out.
- The render is **atomic** (`> *.new && mv`): a failed Doppler fetch aborts under
  `set -e` before overwriting, leaving the last-good secret file in place.
