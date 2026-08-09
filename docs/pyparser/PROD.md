# pyparser on llunde-parser (NixOS)

Production topology: one **Hetzner Cloud CCX23** (4 dedicated vCPU / 16 GiB) named
`llunde-parser` (`hel1`) runs all compute — Postgres, the review server, both queue
workers, cloudflared — as **rootless podman quadlets** under the `pyparser` user
(uid 2001), declared in this repo (`services/pyparser/`, host
`hosts/llunde-parser/`). **AWS S3** is the durable artifact store (dataset bucket)
+ the restic backup destination. **Cloudflare** provides ingress (tunnel) and auth
(Access). The server and the S3/IAM resources are managed by OpenTofu (`tofu/`,
see `README.md`); Access and the tunnel itself are managed out-of-band
(`CLOUDFLARE.md`).

> History: rootful docker-compose under `leploy` until the phase-3.5 NixOS
> reinstall; before that an AWS EC2+RDS setup (decommissioned June 2026). The S3
> bucket + IAM policy are the only AWS pieces that carried through both
> migrations. `leploy`, docker, `/opt/pyparser`, ufw and fail2ban ceased to exist
> with the wipe.

## Host facts
- **SSH**: `root` (ssh alias `pyparser`), **tailnet only** — public port 22 is
  closed (ADR 015; break-glass in `runbook.md` §11). There is no shell workflow
  as the service user: admin work happens as root, reaching into the rootless
  stack with `systemctl --user -M pyparser@ …` for units and
  `runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus podman …` for the
  user's podman.
- **Firewall**: zero public inbound ports of any kind (the Hetzner firewall has
  no rules; the NixOS host firewall opens nothing public). Web ingress is the
  Cloudflare tunnel (outbound); SSH rides the tailnet.
- **Stack**: quadlet units under `pyparser` — `pyparser-postgres`
  (postgres:17-alpine, **pinned**, never auto-updated), `pyparser-migrate`
  (oneshot — see Deploys), `pyparser-review`, `pyparser-worker-extract`,
  `pyparser-worker-light`, `pyparser-cloudflared` — plus the `pyparser` network
  and named volumes `pyparser-pgdata` / `pyparser-files`. All declared in
  `services/pyparser/`; never edit units on the box — change the Nix, redeploy
  (`runbook.md` §6).

## secrets.env — Doppler `pyparser/prd` → `/run/pyparser/secrets.env`

> Source of truth is Doppler project `pyparser`, config `prd` (see
> `../DOPPLER.md`). A **root oneshot** renders the config into
> `/run/pyparser/secrets.env` (0640, group `pyparser`) at boot — ordered by the
> system manager *before* the pyparser user manager starts — and on demand;
> every stack unit consumes it via `EnvironmentFile=`, exactly as compose's
> `env_file` did. Edit secrets with `doppler secrets set … -p pyparser -c prd`,
> then re-run the render oneshot (`systemctl start pyparser-secrets-render`) and restart the
> affected units. The keys the config contains:

```sh
# Postgres (container + apps share these; same password on both lines —
# single-sourced here, there is no separate sops DB-password file)
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

> ⚠️ **Fresh installs restore a dump first.** The alembic chain assumes the
> baseline schema (prod was initialized from `sql/init` and stamped) — an
> `upgrade head` against an EMPTY database fails midway (rehearsal finding).
> The stack's first start on a new box comes AFTER `pg_restore`.

## Deploys — pull-based, zero SSH

Merge to `main` in llunde-pyparser → the build workflow pushes
`ghcr.io/fredrir/pyparser-review:latest` (+ keep-10 GHCR prune) → the box's
per-user auto-update timer (`llunde-auto-update.timer`, every 5 min) sees the
new digest and swaps it. `AutoUpdate=registry` sits on `review` and the two
workers **only**; postgres stays pinned. **Migrations**: `alembic upgrade head`
runs as the `pyparser-migrate` oneshot (same image, command override) *before*
the app units — they `Requires=`/`After=` it, so every restart re-asserts the
schema; alembic is idempotent. Manual poke:
`systemctl --user -M pyparser@ start llunde-auto-update.service`.

**Pin / rollback an image**: set the unit's `Image=` to a digest in
`services/pyparser/`, deploy (`runbook.md` §6/§9) — that IS the rollback lever;
the old SSH deploy workflow with its ERR-trap auto-rollback is retired. Schema
rollback is manual (`alembic downgrade` if the migration has a correct
`downgrade()`, else `pg_restore` from a backup dump).

## Operations
- **Unit sizing** (4-core box; carried over from compose as systemd resource
  caps in `services/pyparser/`): `worker-extract` `CPUQuota=300%` +
  `MemoryMax=6g` + `PYPARSER_WORKER_CONCURRENCY=1` (docling threads 3);
  `worker-light` `MemoryMax=2g`; `TimeoutStopSec` 90s/45s preserves the
  compose-era graceful drain (still > the in-app drain timeout); container
  healthchecks are **off** on both workers (the image's `curl :8081/healthz`
  HEALTHCHECK only fits the review web service; `review` keeps it).
- **Backups** (restic, `modules/backups/pyparser.nix`): weekly system timer;
  a preHook `pg_dump -Fc` stages `/var/backup/pyparser/pyparser_llunde.dump`,
  then restic snapshots it together with the `pyparser-files` volume data to
  `s3://llunde-pyparser-bucket/restic/llunde-parser`. Force a run:
  `systemctl start restic-backups-pyparser.service` (root, on the box).
  Restore: `restic restore latest --target /tmp/restore`, then `pg_restore` the
  dump into `pyparser-postgres` and copy files back into the volume
  (`runbook.md` §12). The old `s3://llunde-pyparser-bucket/pg-backups/` prefix
  is a **frozen archive** of the compose-era nightly dumps — read-only history,
  nothing writes there anymore.
- **Queue health**: `/api/queue/depth`, `/api/queue/failed`; the reaper on
  `worker-light` requeues / dead-letters stuck jobs.
- **Logs**: journald, over the tailnet — **Dozzle is retired** along with
  `socket-proxy` (`parser.llunde.no/logs` is gone; phase-3.5 decision 3 — phase
  4 observability replaces the browser view). As root:
  `journalctl _UID=2001 -e` for the whole stack,
  `journalctl _SYSTEMD_USER_UNIT=pyparser-review.service` per unit, or
  `journalctl CONTAINER_NAME=pyparser-review` for a container's own output.
  Unit status: `systemctl --user -M pyparser@ status pyparser-review` (or
  `list-units 'pyparser-*'`).
- **Tuning**: `PYPARSER_EXTRACT_WATCHDOG_TIMEOUT_S` for a hard per-job wall
  clock; a third worker lane is config-only — a new unit in `services/pyparser/`
  with `PYPARSER_WORKER_STAGES=STRUCTURE_TASK`.
- **Admin DB access (laptop)** — prod Postgres still has no public port, so
  `pyparser-sync` reaches it over an SSH local-forward. On your laptop set
  `PYPARSER_PROD_DATABASE_URL=postgres://pyparser:<pw>@postgres:5432/pyparser_llunde`
  (user/pw/dbname real; host/port are placeholders the tunnel rewrites) +
  `PYPARSER_PROD_SSH_HOST=pyparser`, then e.g. `pyparser-sync db diff dev prod`
  or `pyparser-sync db pull prod`. `ssh pyparser` must work non-interactively
  (ssh agent or keyfile — the tunnel uses `BatchMode`). **Known limitation**:
  `pyparser-sync`'s auto-discovery shells out to `docker inspect` on the host,
  and docker no longer exists — the tool needs a small update in the
  llunde-pyparser repo (podman equivalent, run as the pyparser user:
  `runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus podman inspect pyparser-postgres`).
  Until then, resolve the IP manually with that command and point the tunnel at it.
- **OpenTofu**: `doppler run --project pyparser --config prd -- tofu -chdir=tofu plan`
  should report "No changes." — the reinstall was invisible to the cloud API.
  The server is `prevent_destroy` + Hetzner `delete_protection`.
