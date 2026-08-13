# pyparser on llunde-parser (NixOS)

A single Hetzner Cloud **CCX23** (4 dedicated vCPU / 16 GiB, `hel1`) named
`llunde-parser` runs all compute as **rootless podman quadlets** under the
`pyparser` user (uid 2001), declared in `services/pyparser/` and
`hosts/llunde-parser/`. **AWS S3** is the dataset store and restic backup
destination; **Cloudflare** provides ingress (tunnel) and auth (Access). Server
and S3/IAM are OpenTofu's ([README.md](README.md)); Access and the tunnel are
out-of-band ([CLOUDFLARE.md](CLOUDFLARE.md)).

> History: rootful docker-compose under `leploy` until the NixOS reinstall, and
> AWS EC2+RDS before that (decommissioned June 2026). The S3 bucket + IAM policy
> are the only AWS pieces that survived both migrations; `leploy`, docker,
> `/opt/pyparser`, ufw and fail2ban died with the wipe.

## Host facts

- **SSH**: `root` (ssh alias `pyparser`), **tailnet only** — public port 22 is
  closed (ADR 015; break-glass in [../runbook.md](../runbook.md)). There is no
  shell workflow as the service user. Admin work happens as root and reaches
  into the rootless stack two ways — `systemctl --user -M pyparser@ …` for
  units, and for the user's podman:

  ```sh
  runuser -u pyparser -- env XDG_RUNTIME_DIR=/run/user/2001 \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2001/bus podman …
  ```

- **Firewall**: zero public inbound ports of any kind — the Hetzner firewall has
  no rules and the NixOS firewall opens nothing public. Web ingress is the
  Cloudflare tunnel (outbound); SSH rides the tailnet.
- **Stack**: `pyparser-postgres` (postgres:17-alpine, **pinned**, never
  auto-updated), `pyparser-migrate` (oneshot, see Deploys), `pyparser-review`,
  `pyparser-worker-extract`, `pyparser-worker-light`, `pyparser-cloudflared`,
  plus the `pyparser` network and volumes `pyparser-pgdata` / `pyparser-files`.
  Never edit units on the box: change the Nix and redeploy
  ([../runbook.md](../runbook.md)).

## secrets.env — Doppler `pyparser/prd` → `/run/pyparser/secrets.env`

Source of truth is Doppler project `pyparser`, config `prd`
([../doppler.md](../doppler.md)). A **root oneshot** renders it to
`/run/pyparser/secrets.env` (0640, group `pyparser`) at boot — ordered before the
pyparser user manager starts — and on demand; every stack unit reads it via
`EnvironmentFile=`. To change a secret: `doppler secrets set … -p pyparser -c
prd`, then `systemctl start pyparser-secrets-render` and restart the affected
units. The keys:

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
> `upgrade head` against an EMPTY database fails midway (rehearsal finding). The
> stack's first start on a new box comes AFTER `pg_restore`.

## Deploys — pull-based, zero SSH

Merge to `main` in llunde-pyparser → the build workflow pushes
`ghcr.io/fredrir/pyparser-review:latest` (+ keep-10 GHCR prune) → the per-user
auto-update timer (`llunde-auto-update.timer`, every 5 min) sees the new digest
and swaps it. `AutoUpdate=registry` is on `review` and the two workers **only**;
postgres stays pinned. **Migrations**: `alembic upgrade head` runs as the
`pyparser-migrate` oneshot (same image, command override) *before* the app
units, which `Requires=`/`After=` it — every restart re-asserts the schema, and
alembic is idempotent. Manual poke: `systemctl --user -M pyparser@ start
llunde-auto-update.service`.

**Pin / rollback an image**: set the unit's `Image=` to a digest in
`services/pyparser/` and deploy — that IS the rollback lever, replacing the
retired SSH deploy workflow's ERR-trap auto-rollback. Schema rollback is manual
(`alembic downgrade` if the migration has a correct `downgrade()`, else
`pg_restore` from a backup dump).

## Operations

- **Unit sizing** (4-core box; compose's caps carried over as systemd resource
  limits): `worker-extract` `CPUQuota=300%` + `MemoryMax=6G` +
  `PYPARSER_WORKER_CONCURRENCY=1` (docling threads 3); `worker-light`
  `MemoryMax=2G`; `TimeoutStopSec` 90s/45s preserves the graceful drain, still
  longer than the in-app drain timeout. Container healthchecks are **off** on
  both workers — the image's `curl :8081/healthz` HEALTHCHECK fits only the
  review web service, which keeps it.
- **Backups** (restic, `modules/backups/pyparser.nix`): weekly system timer; a
  preHook `pg_dump -Fc` stages `/var/backup/pyparser/pyparser_llunde.dump`, then
  restic snapshots it with the `pyparser-files` volume data to
  `s3://llunde-pyparser-bucket/restic/llunde-parser`. Force a run as root:
  `systemctl start restic-backups-pyparser.service`. Restore: `restic restore
  latest --target /tmp/restore`, then `pg_restore` the dump into
  `pyparser-postgres` and copy files back into the volume
  ([../runbook.md](../runbook.md)). The old
  `s3://llunde-pyparser-bucket/pg-backups/` prefix is a **frozen archive** of the
  compose-era nightly dumps — nothing writes there anymore.
- **Queue health**: `/api/queue/depth`, `/api/queue/failed`; the reaper on
  `worker-light` requeues / dead-letters stuck jobs.
- **Logs**: journald over the tailnet. **Dozzle is retired** with `socket-proxy`
  (`parser.llunde.no/logs` is gone; the observability stack replaces it). As
  root: `journalctl _UID=2001 -e` (whole stack),
  `journalctl _SYSTEMD_USER_UNIT=pyparser-review.service` (per unit),
  `journalctl CONTAINER_NAME=pyparser-review` (a container's own output). Status:
  `systemctl --user -M pyparser@ status pyparser-review`, or `list-units
  'pyparser-*'`.
- **Tuning**: `PYPARSER_EXTRACT_WATCHDOG_TIMEOUT_S` sets a hard per-job wall
  clock; a third worker lane is config-only — a new unit in `services/pyparser/`
  with `PYPARSER_WORKER_STAGES=STRUCTURE_TASK`.
- **Admin DB access (laptop)**: prod Postgres has no public port, so
  `pyparser-sync` reaches it over an SSH local-forward. Set
  `PYPARSER_PROD_DATABASE_URL=postgres://pyparser:<pw>@postgres:5432/pyparser_llunde`
  (user/pw/dbname real; host/port are placeholders the tunnel rewrites) and
  `PYPARSER_PROD_SSH_HOST=pyparser`, then e.g. `pyparser-sync db diff dev prod`.
  `ssh pyparser` must work non-interactively (agent or keyfile — the tunnel uses
  `BatchMode`). **Known limitation**: its auto-discovery shells out to `docker
  inspect`, and docker no longer exists. Until the llunde-pyparser repo gains a
  podman equivalent, resolve the IP by hand with the `runuser … podman inspect
  pyparser-postgres` form above and point the tunnel at it.
- **OpenTofu**: `doppler run --project pyparser --config prd -- tofu -chdir=tofu plan`
  should report "No changes." — the reinstall was invisible to the cloud API. The
  server is `prevent_destroy` + Hetzner `delete_protection`.
