# llunde-parser tenancy mapping — snapshot 2026-08-09

> **Historical.** Ground truth gathered 2026-08-09 from full reads of the
> llunde-pyparser repo (local clone) and the portfolio repo
> (github.com/fredrir/portfolio), plus live DNS/availability probes. It
> describes the box *before* its NixOS reinstall — for today see the
> [repo README](../../README.md), [docs/runbook.md](../runbook.md) and
> [docs/pyparser/PROD.md](../pyparser/PROD.md). Kept because
> [ADR 015](../decisions/015-tailnet-only-management.md) and
> [ADR 016](../decisions/016-tenant-slots-host-uniformity.md) rest on it.

## Headline facts

1. **portfolio is live in production on `llunde-parser`** (hansteen.dev, since
   2026-07) — the box hosts **two** productions, not one.
2. **llunde-pyparser has no CI.** No `.github/` existed and none ever did — the
   deploy workflow lived in the old monorepo and died with it. The preserved
   reference is
   [old-workflows/old.deploy-pyparser.yml](old-workflows/old.deploy-pyparser.yml):
   push-to-main → build `review-serve` → GHCR → CI SSHes in as `leploy` and runs
   `deploy-remote.sh` (Doppler-renders `secrets.env` → pre-migrate `pg_dump` →
   drain workers → `alembic upgrade head` → `up -d --wait` → healthz → ERR-trap
   rollback), with `workflow_dispatch`'s `image_tag` as the pin/rollback lever
   and a keep-10 GHCR prune. Manual SSH was break-glass only.

## llunde-pyparser (the app repo)

FastAPI + uvicorn (Python 3.11+, heavy CPU-ML deps: torch, docling with ~7 model
bundles baked into the image at build), React/Vite SPA served by the same
`review-serve` image. Alembic, 37 revisions, applied at deploy time (not
startup). One image, `ghcr.io/fredrir/pyparser-review`, runs review + both
workers.

`docker-compose.prod.yml` — 8 services, no published host ports, one host bind
mount total: `postgres:17-alpine` (volume `pyparser-pgdata`), `review` (:8081
internal), `worker-extract` (mem 6g / cpus 3.0 / `stop_grace_period` 90s > drain
timeout — job-fencing by design), `worker-light` (reaper), `cloudflared`
(token-only, remote-managed tunnel), `socket-proxy` (**the** bind mount:
`/var/run/docker.sock:ro`) feeding `dozzle` (`/logs`, behind CF Access), and
`db-backup` + `backup-ship` (nightly dump →
`s3://llunde-pyparser-bucket/pg-backups/`, implemented as `sleep 86400`
while-loops in containers, not timers).

Secrets: Doppler project `pyparser` was the single source of truth;
`deploy-remote.sh` rendered `/opt/pyparser/secrets.env` (umask 077) from `prd`
on every deploy, and 7 services consumed it via `env_file`. Hard startup guards
in prod: CF Access team domain/AUD, admin emails, built SPA present.

Frictions identified for the rootless/NixOS reinstall: the docker-socket proxy
for Dozzle; a deploy script docker-CLI-shaped throughout (`compose --wait`,
retag-rollback); containers running as root internally (no `USER`);
`pyparser-sync`'s laptop DB access resolving the container IP via `ssh … docker
inspect`; compose knobs (`mem_limit`, `shm_size: 256m`, `stop_grace_period`)
needing quadlet/systemd equivalents; backup sleep-loops becoming timers.

Hygiene notes: stale monorepo pointers in `CLAUDE.md`/`README.md`/`.envrc`
(paths above repo root); the committed `backups/` dumps and
`scripts/promote_dev_to_prod.sh` are owner-acknowledged and out of scope.

## portfolio (the second tenant)

Rust axum API + SQS worker + TanStack Start SSR web, running as user `portfolio`
— and already implementing the tenant-slot model end-to-end: subuids
`200000-265535`, linger, rootless podman quadlets in
`~/.config/containers/systemd/` (own network; postgres 17 + pgbouncer +
wal-receiver + caddy + cloudflared + blue/green api/web pairs + worker), zero
published host ports, own outbound CF tunnel, Caddy slot-switching for
blue/green.

Deploys: GitHub CI builds three images → GHCR (provenance + SBOM + cosign
keyless), then `ssh portfolio@$DEPLOY_HOST "deploy <sha>"` — a **forced-command
over public port 22**, which is why closing 22 required a tailnet path first.
Host-side `deploy.sh` cosign-verifies against the repo identity, pulls, flips
slots with health gates and auto-rollback. Secrets come from its own Doppler
project via `render-env.sh` (needs `doppler` + `python3` on the host, installed
imperatively at the time). Backups: nightly + weekly basebackup + WAL shipping
every 15 min to its own S3 bucket, weekly restore-test timer. Synthetic checks
probe hansteen.dev every 30 min.

Its Ubuntu-specific `infra/host/bootstrap.sh` (apt/adduser/usermod/linger +
podman/cosign) is exactly what `modules/tenants/` came to express declaratively.
Its `docs/vps-audit.md` documents pyparser as co-tenant and commits to
non-interference; its ADRs forbid its terraform from managing the shared server.

**`hansteen.dev` is portfolio's own Cloudflare zone**, managed by its own
terraform in the same CF account — outside llunde-infra's scope permanently.

## The box and the edges

`llunde-parser` then: Hetzner CCX23, Ubuntu (imperative), inbound 22 only
(Hetzner fw + ufw + fail2ban), all web ingress via the two CF tunnels, **no
Tailscale yet**. Tenants: `leploy` (pyparser, rootful docker) + `portfolio`
(rootless podman). SSH consumers: the laptop (`letzner` alias, including
`pyparser-sync`'s tunnel) and portfolio CI.

DNS on 2026-08-09: `llunde.no`/`www`/`api` grey-cloud A → 46.62.214.182
(`llunde-01`); `parser`/`external` proxied CNAMEs → the pyparser tunnel. The
retired llunde tunnel (`ed8abcdb…`) carried no traffic but its deletion was
unverified — checked during the zone import.

`llunde-pyparser-bucket` serves triple duty: pyparser dataset (+`pg-backups/`),
tofu state (`tofu-state/`), restic (`restic/…`).
