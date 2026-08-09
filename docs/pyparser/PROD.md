# pyparser on Hetzner

Production topology: one **Hetzner Cloud CCX23** (4 dedicated vCPU / 15 GiB) named
`llunde-parser` (`hel1`) runs all compute — Postgres, the review server, both queue
workers, cloudflared — via Docker Compose. **AWS S3** is the durable artifact store
(dataset bucket) + the off-box destination for nightly DB backups. **Cloudflare**
provides ingress (tunnel) and auth (Access). The server, its network firewall, and
the S3/IAM resources are managed by Terraform (see `README.md`); Cloudflare is
managed out-of-band (`CLOUDFLARE.md`).

> History: this replaced an AWS EC2+RDS setup in June 2026. Data was migrated from
> RDS (`pg_dump` → restore into the in-container Postgres); RDS + EC2 were
> decommissioned. The S3 bucket + IAM policy are the only AWS pieces that carried over.

## Host facts
- **SSH**: `root` (ssh alias `pyparser`, over the tailnet) for admin; `leploy` (alias `leploy`, in the
  `docker` group) owns `/opt/pyparser` and runs the stack. Key-only
  (`PasswordAuthentication no`); root is `prohibit-password`.
- **Firewall**: Hetzner Cloud firewall (inbound SSH-only, Terraform-managed) + in-OS
  `ufw` (allow 22) + `fail2ban`. No inbound web ports — ingress is the Cloudflare
  tunnel (outbound).
- **Stack**: `/opt/pyparser/docker-compose.yml` (shipped by the deploy workflow
  from this repo's `../../pyparser/docker-compose.prod.yml`) + `/opt/pyparser/secrets.env`
  (`chmod 600`, owner `leploy`; **rendered from Doppler config `prd` on each deploy**
  by `deploy-remote.sh` — Doppler is the source of truth, see `../DOPPLER.md`).

## secrets.env (host, chmod 600) — now rendered from Doppler (`prd`)

> Source of truth is Doppler project `pyparser`, config `prd` (see `../DOPPLER.md`).
> `deploy-remote.sh` regenerates this file from Doppler before every `docker compose`
> call. Edit secrets with `doppler secrets set … -p pyparser -c prd`, not by hand.
> The keys below are what that config contains:

```sh
# Postgres (container + apps share these; same password on both lines)
POSTGRES_PASSWORD=...
PYPARSER_LLUNDE_DATABASE_URL=postgres://pyparser:...@postgres:5432/pyparser_llunde
# AWS — S3 only (leploy IAM user key + pyparser-dataset policy)
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_DEFAULT_REGION=eu-north-1
PYPARSER_S3_DATASET_BUCKET=llunde-pyparser-bucket
# Review server / Cloudflare Access (values in CLOUDFLARE.md)
PYPARSER_ADMIN_EMAILS=["..."]
PYPARSER_CF_ACCESS_TEAM_DOMAIN=hidden-pond-1118.cloudflareaccess.com
PYPARSER_CF_ACCESS_AUD=...
# Cloudflare tunnel — the token embeds the tunnel secret; do NOT rotate casually
TUNNEL_TOKEN=...
```

## Deploys
The `Deploy pyparser` GitHub workflow (SSH to the host, secrets
`PYPARSER_HETZNER_HOST` + `PYPARSER_HETZNER_SSH_KEY`): build + push the image →
`scp docker-compose.prod.yml` to the host → capture rollback anchors (current
image + `alembic current`) → **pre-migration `pg_dump`** into `/opt/pyparser/pre-migrate/`
(last 5 kept) → `docker compose stop worker-extract worker-light review` (graceful
drain — in-flight jobs finish or are fenced back to PENDING) → `alembic upgrade head`
→ `up -d --wait` → `healthz`. Any failure trips an `ERR` trap that runs
`alembic downgrade <prev>`, re-tags the previous image as `:latest`, and brings the
old stack back up. **Rollback caveat:** schema rollback only works if the migration
has a correct `downgrade()`; otherwise restore from the pre-migrate dump
(`pg_restore --clean --if-exists -d "$DSN" pre-migrate/<tag>.dump`).
Keep `:latest` = `main`; do **not** deploy a parser-stage-rework image until its
migrations (0019+) are intended for prod.

## Operations
- **Compose sizing** (4-core box, set in `docker-compose.prod.yml`): `worker-extract`
  `cpus: 3.0` + `PYPARSER_WORKER_CONCURRENCY: 1`; both worker services have
  `healthcheck: { disable: true }` (the image's `curl :8081/healthz` HEALTHCHECK only
  fits the review web service).
- **Backups**: `db-backup` dumps nightly (14 kept locally); `backup-ship` mirrors to
  `s3://llunde-pyparser-bucket/pg-backups/`. Restore: bring up postgres, then
  `gunzip -c dump.sql.gz | docker compose exec -T postgres psql -U pyparser -d pyparser_llunde`.
- **Queue health**: `/api/queue/depth`, `/api/queue/failed`; the reaper on
  `worker-light` requeues / dead-letters stuck jobs.
- **Logs (browser)**: Dozzle at `https://parser.llunde.no/logs` (admin Access) — live
  container logs without SSH. It reads the daemon through a read-only
  `socket-proxy` (the host socket is never mounted into Dozzle). `docker logs` over
  SSH stays the fallback.
- **Tuning**: `PYPARSER_EXTRACT_WATCHDOG_TIMEOUT_S` for a hard per-job wall clock; a
  third lane is config-only (a service with `PYPARSER_WORKER_STAGES=STRUCTURE_TASK`).
- **Admin DB access (laptop)** — prod Postgres has no public port (SSH-only firewall),
  so `pyparser-sync` reaches it over an SSH local-forward. On your laptop set
  `PYPARSER_PROD_DATABASE_URL=postgres://pyparser:<pw>@postgres:5432/pyparser_llunde`
  (user/pw/dbname real; host/port are placeholders the tunnel rewrites) +
  `PYPARSER_PROD_SSH_HOST=pyparser`, then e.g. `pyparser-sync db diff dev prod` or
  `pyparser-sync db pull prod`. `ssh pyparser` must work non-interactively (ssh agent
  or keyfile — the tunnel uses `BatchMode`). Override per-run with `--ssh <host>`.
- **Terraform**: `export TF_VAR_hcloud_token=…; terraform plan` should report
  "No changes." The server is `prevent_destroy` + Hetzner `delete_protection` — see
  `README.md` for the adoption/import model.
```

