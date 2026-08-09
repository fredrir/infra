# Phase-3 mapping — llunde-pyparser, portfolio, and the shared box

Ground truth gathered 2026-08-09, after the phase-2 gate closed, from full reads of the llunde-pyparser repo (local clone) and the portfolio repo (github.com/fredrir/portfolio), plus live DNS/availability probes. This is what the [phase-3](../init/plans/phase-3/README.md) and [phase-3.5](../init/plans/phase-3.5/README.md) plans rest on. Facts here describe *today*; where a phase changes one, that phase's docs win.

## The headline facts

1. **portfolio is live in production on `llunde-parser`** (hansteen.dev, since 2026-07) — the box hosts **two** productions, not one.
2. **llunde-pyparser has no CI.** No `.github/` exists and none ever did — the deploy workflow lived in the old monorepo and died with it. The preserved reference is [old-workflows/old.deploy-pyparser.yml](old-workflows/old.deploy-pyparser.yml): push-to-main → build `review-serve` → GHCR → CI SSHes in as `leploy` and runs `deploy-remote.sh` (Doppler-renders `secrets.env` → pre-migrate `pg_dump` → drain workers → `alembic upgrade head` → `up -d --wait` → healthz → ERR-trap rollback), with a `workflow_dispatch` `image_tag` input as the pin/rollback lever and a keep-10 GHCR prune. Manual SSH was break-glass only.

## llunde-pyparser (the app repo)

FastAPI + uvicorn (Python 3.11+, heavy CPU-ML deps: torch, docling with ~7 model bundles baked into the image at build), React/Vite SPA served by the same `review-serve` image. Alembic, 37 revisions, applied at deploy time (not startup — the 3.5 convergence point). Single image `ghcr.io/fredrir/pyparser-review` runs review + both workers.

`docker-compose.prod.yml` — 8 services, no published host ports, no host bind mounts except one: `postgres:17-alpine` (volume `pyparser-pgdata`), `review` (:8081 internal), `worker-extract` (mem 6g / cpus 3.0 / `stop_grace_period` 90s > drain timeout — job-fencing by design), `worker-light` (reaper), `cloudflared` (token-only, remote-managed tunnel), `socket-proxy` (**the** bind mount: `/var/run/docker.sock:ro`) feeding `dozzle` (`/logs`, behind CF Access), `db-backup` + `backup-ship` (nightly dump → `s3://llunde-pyparser-bucket/pg-backups/`, implemented as `sleep 86400` while-loops in containers, not timers).

Secrets: Doppler project `pyparser` is the single source of truth; `deploy-remote.sh` renders `/opt/pyparser/secrets.env` (umask 077) from config `prd` on every deploy; 7 services consume it via `env_file`. Hard startup guards in prod: CF Access team domain/AUD, admin emails, built SPA present.

Rootless/NixOS frictions (3.5 concerns, not phase-3): docker-socket proxy for Dozzle; deploy script is docker-CLI-shaped throughout (`compose --wait`, retag-rollback); containers run as root internally (no `USER`); `pyparser-sync`'s laptop DB access resolves the container IP via `ssh … docker inspect`; compose knobs (`mem_limit`, `shm_size: 256m`, `stop_grace_period`) need quadlet/systemd equivalents; backup sleep-loops become timers.

Repo hygiene notes: stale monorepo pointers in `CLAUDE.md`/`README.md`/`.envrc` (paths above repo root); the committed `backups/` dumps and `scripts/promote_dev_to_prod.sh` are owner-acknowledged and explicitly out of scope.

## portfolio (the second tenant)

Rust axum API + SQS worker + TanStack Start SSR web, deployed on `llunde-parser` as user `portfolio` — and it already implements the tenant-slot model end-to-end: subuids `200000-265535`, linger, rootless podman quadlets in `~/.config/containers/systemd/` (own network; postgres 17 + pgbouncer + wal-receiver + caddy + cloudflared + blue/green api/web pairs + worker), zero published host ports, own outbound CF tunnel, Caddy slot-switching for blue/green.

Deploys: GitHub CI builds three images → GHCR (provenance + SBOM + cosign keyless), then `ssh portfolio@$DEPLOY_HOST "deploy <sha>"` — a **forced-command over public port 22** (the phase-3 coupling: closing 22 without a tailnet path breaks this). Host-side `deploy.sh` cosign-verifies against the repo identity, pulls, flips slots with health gates and auto-rollback. Secrets via its own Doppler project rendered by `render-env.sh` (needs `doppler` + `python3` on the host — installed imperatively today). Backups: nightly + weekly basebackup + WAL shipping every 15 min to its own S3 bucket, restore-test timer weekly. Synthetic checks probe hansteen.dev every 30 min.

Its Ubuntu-specific `infra/host/bootstrap.sh` (apt/adduser/usermod/linger + podman/cosign) is exactly what `modules/tenants/` must express declaratively in 3.5. Its `docs/vps-audit.md` documents pyparser as co-tenant and commits to non-interference; its ADRs forbid its terraform from managing the shared server.

**`hansteen.dev` is portfolio's own Cloudflare zone**, managed by its own terraform (same CF account) — out of llunde-infra's scope permanently.

## The box and the edges

`llunde-parser`: Hetzner CCX23, Ubuntu (imperative), inbound 22 only (Hetzner fw + ufw + fail2ban), all web ingress via the two CF tunnels, **no Tailscale yet**. Tenants: `leploy` (pyparser, rootful docker) + `portfolio` (rootless podman). SSH consumers today: laptop (`letzner` alias, incl. `pyparser-sync`'s tunnel), portfolio CI; pyparser CI joins as third when recreated.

DNS reality (2026-08-09): `llunde.no`/`www`/`api` grey-cloud A → 46.62.214.182 (`llunde-01`, phase-2 cutover held); `parser`/`external` proxied CNAMEs → pyparser tunnel. The retired llunde tunnel (`ed8abcdb…`) carries no traffic but its **deletion is unverified** — phase-3 zone import checks it.

`llunde-pyparser-bucket` now serves triple duty: pyparser dataset (+`pg-backups/`), tofu state (`tofu-state/`), restic (`restic/llunde-01`). Prefix layout is documented when the pyparser root's state joins (phase-3 step 1); consolidation questions, if any, belong to 3.5+.
