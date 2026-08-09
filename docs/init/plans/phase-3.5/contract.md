# Phase 3.5 — llunde-parser contract

Frozen values the streams build against. Anything here that proves wrong at execution is a contract bug — fix it here first, then the code.

## Host

| | |
|---|---|
| Hostname / tailnet name | `llunde-parser` (unchanged — decision 1) → `llunde-parser.tail0b6cbe.ts.net` |
| Server | Hetzner 141119325, CCX23 (4 dedicated vCPU / 16G / 152.6G `/dev/sda`), hel1 |
| Boot | **UEFI** → systemd-boot, disko ext4 wipe (llunde-01 pattern; 512M ESP) |
| Public inbound | **none** (firewall has zero rules; ingress = CF tunnels outbound; SSH = tailnet) |
| Profiles | same `modules/profiles/server.nix` — with `allowedTCPPorts` becoming a per-host option (llunde-01: 80/443; llunde-parser: none) |

## Users

| User | uid | subuid | Role |
|---|---|---|---|
| `pyparser` | 2001 | `165536:65536` (**explicit** — old leploy's range) | rootless podman, runs the whole pyparser stack |
| `portfolio` | 3000 | `100000:65536` (**explicit** — matches current reality) | tenant slot (ADR 016): user + linger + deps only |

Both ranges are declared explicitly, never auto: NixOS auto-allocation starts at 100000 and would collide with portfolio's declared range. Old `leploy` (1001) and docker cease to exist by construction. Tenant slots use the 3000+ block; 20xx stays for llunde-infra-managed services.

## pyparser stack — compose → quadlet mapping

Network `pyparser` (one per-user network, non-Internal — go-live fix 7). The four app units (`review`, both workers, `migrate`) run `ghcr.io/fredrir/pyparser-review:latest`; **`AutoUpdate=registry` goes on `review` and the two workers only** — postgres stays pinned (unattended DB image swaps are a data hazard) and `migrate` carries no label (oneshot `--rm` containers confuse `podman auto-update`; it rides along via `Requires=` ordering when the apps restart). **Every unit gets `EnvironmentFile=/run/pyparser/secrets.env`** — the exact set compose's `env_file` covers today — and the render oneshot is ordered by the *system* manager before `user@2001.service` starts (user units cannot `After=` system units; this ordering is the reboot test's explicit proof).

| Unit | From compose service | Carry-over knobs |
|---|---|---|
| `pyparser-postgres` | `postgres` (postgres:17-alpine) | volume `pyparser-pgdata`; `shm_size 256m` → `ShmSize=`; run as own uid for `:U` (go-live fix 5); healthcheck pg_isready |
| `pyparser-migrate` | *(new, oneshot)* | same image, `Exec=alembic upgrade head`; `After=pyparser-postgres` + healthy; app units `Requires=`+`After=` it |
| `pyparser-review` | `review` | :8081 on the pod network only; `PYPARSER_DOCLING_NUM_THREADS=1`; HealthCmd curl healthz |
| `pyparser-worker-extract` | `worker-extract` | `MemoryMax=6g` `CPUQuota=300%` `TimeoutStopSec=90`; stages EXTRACT, concurrency 1, docling threads 3, doctr resident; healthcheck off |
| `pyparser-worker-light` | `worker-light` | `MemoryMax=2g` `TimeoutStopSec=45`; stages CLASSIFY,BATCH,SPLIT,STRUCTURE_TASK; reaper on |
| `pyparser-cloudflared` | `cloudflared` | token-only connector; `TUNNEL_TOKEN` from the env file; **masked on rehearsal boxes** |
| *(dropped)* | `socket-proxy`, `dozzle` | decision 3 — journald replaces the /logs view |
| *(dropped)* | `db-backup`, `backup-ship` | decision 4 — modules/backups (restic) replaces both |

Volumes: `pyparser-pgdata`, `pyparser-files` (named, per-user). `pyparser-pgbackups` retires with the backup containers.

## Secrets (sops-nix, four files — llunde-01 pattern, two-host wiring)

| File | Key(s) | Content |
|---|---|---|
| `secrets/pyparser-doppler.yaml` | `doppler_token` (env form) | fresh read-only service token, Doppler `pyparser/prd` |
| `secrets/tailscale.yaml` | shared file, re-encrypted | fresh auth key for the reinstall |
| `secrets/pyparser-restic.yaml` | `password`, `env` | new restic password; leploy AWS creds (bucket rights suffice) |
| `secrets/ghcr.yaml` | shared file, re-encrypted | same read-only PAT already pulling private packages on llunde-01 |

No sops DB-password file: `POSTGRES_PASSWORD` is **single-sourced** from the Doppler render (the postgres unit consumes the same `EnvironmentFile` as everything else, exactly like compose today). Two-host sops wiring is real work owned by stream A: per-host creation rules in `.sops.yaml`, `sops updatekeys` on the shared files once the llunde-parser host key exists (pre-generated in step 0), and `modules/secrets` parameterized per host instead of hardcoding llunde-01's set.

Runtime env: a root oneshot renders Doppler `pyparser/prd` → `/run/pyparser/secrets.env` (0640, group `pyparser`) at boot and on demand. Restic: `s3://llunde-pyparser-bucket/restic/llunde-parser` (llunde-01 uses `restic/llunde-01`).

## CI / deploy

- llunde-pyparser `deploy.yml` → `build.yml`: build + push + keep-10 prune on push-to-main (GITHUB_TOKEN only — no Doppler); `workflow_dispatch` retained for manual builds. Deploy = auto-update on the box. Pin/rollback = `Image=<digest>` in `services/pyparser/`, rebuild.
- Retired after cutover: Doppler `pyparser/ci` keys `PYPARSER_HETZNER_HOST`, `PYPARSER_HETZNER_SSH_KEY`, `TAILSCALE_AUTHKEY`, **and** the now-unused `DOPPLER_TOKEN_PYPARSER_CI` GitHub repo secret.
- portfolio CI: workflow untouched (tailnet deploy as of phase 3), but the reinstall's new host key means `DEPLOY_KNOWN_HOSTS` must be re-scanned — **set with `-e Production`** (environment secrets shadow repo secrets; phase-3 lesson).

## Data migration (sizes: pgdata 98M, files 308M — trivial transfers)

pyparser: final `pg_dump -Fc` + `tar` of the files volume → laptop + S3 before wipe; restore = psql/pg_restore into the new postgres unit + untar into `pyparser-files`. portfolio: restores itself from its own S3 backups per its DR runbook (RPO 24h / RTO 2h; fresh `backup.sh` run forced pre-wipe).
