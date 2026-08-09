# Phase 2 — frozen port/user/image contract

Step-0 freeze (tasks.md 0.1). Streams implement against these values; changing one
is a lead decision recorded here.

## Users (fixed uids — quadlet paths depend on them)

| User | uid | Runs |
|---|---|---|
| `edge` | 2000 | Caddy (only public ports) |
| `llunde-backend` | 2001 | backend + postgres + valkey |
| `llunde-frontend` | 2002 | frontend static server |
| (reserved) `openclaw` | 2010 | not managed by this repo |

## Network & ports

- Public: Caddy 80/443 only (`ip_unprivileged_port_start=80`).
- Loopback contract (cross-user, rootless networks cannot span users):
  backend API `127.0.0.1:8080` · frontend `127.0.0.1:8081`.
- Per-user podman network `llunde-backend-data` (internal): backend ↔ postgres ↔ valkey; nothing published except the API port.
- Tailnet-only: node metrics + app `/metrics` (via Caddy tailnet listener or direct port bound to the tailscale IP — stream C/D pick, document).

## Images

| Unit | Image | Update |
|---|---|---|
| backend | `ghcr.io/fredrir/llunde-backend:latest` | `AutoUpdate=registry` |
| frontend | `ghcr.io/fredrir/llunde-frontend:latest` | `AutoUpdate=registry` |
| postgres | `docker.io/library/postgres:17` | pinned major, manual |
| valkey | `docker.io/valkey/valkey:8` | pinned major, manual; `--appendonly yes` |
| caddy | `docker.io/library/caddy:2` | pinned major, manual |

## Resource caps (4 GB box, declared in units)

- backend: `JAVA_OPTS=-Xmx640m` (+ the baked prod flags); systemd `MemoryMax=1G`
- postgres: `shared_buffers=256MB`, `MemoryMax=768M`
- valkey: `maxmemory 192mb` (+ `maxmemory-policy noeviction` — sessions must not evict), `MemoryMax=384M`
- caddy/frontend: `MemoryMax=256M` each

## Secrets flow (lead decision — extends ADR 007)

- sops-nix holds: Doppler service token (env-file form: `DOPPLER_TOKEN=...`), Tailscale auth key, restic password **plus restic AWS credentials** (`secrets/restic.yaml` carries `password` and `env` — the env key in env-file form with `AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_DEFAULT_REGION`; v1 reuses the existing `leploy` object-RW credentials, a dedicated backup IAM user is a recorded hardening follow-up), **`ghcr.yaml`** (`auth_json` = containers-auth.json with a `read:packages` PAT — images are PRIVATE by owner decision, pulled via `REGISTRY_AUTH_FILE`), **and `llunde-backend-db.env`** (`POSTGRES_PASSWORD` = backend's `DB_PASSWORD`, plus `VALKEY_PASSWORD` if set). Rationale: postgres/valkey containers cannot wrap Doppler; one source for infra-internal creds beats two drifting ones. ADR 007 gets a consequences note at go-live.
- Backend container: entrypoint `doppler run --` (CLI baked into the image, `DOPPLER_TOKEN` env from sops) for app-level secrets; DB/Valkey creds via the shared sops env file.
- App env (quadlet-set): `APP_ENV=prod`, `DB_HOST=llunde-postgres` (container name on the data network), `DB_PORT=5432`, `DB_NAME=llunde`, `DB_USER=llunde`, `VALKEY_HOST=llunde-valkey`, `VALKEY_PORT=6379`, `CORS_ALLOWED_ORIGINS=https://llunde.no`.

## DNS / hosts

- `llunde-01` (Hetzner rename at apply): tailnet name `llunde-01`.
- Cutover records (grey-cloud): `llunde.no` A 46.62.214.182 / AAAA `2a01:4f9:c014:cbe0::1`, `api.llunde.no` and `www.llunde.no` same (www redirects to apex at Caddy); tunnel CNAME removed after.
