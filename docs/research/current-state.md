# Current state — repo and real infrastructure

Audit of the transferred `llunde-infra` repo and the machines it describes, as of 2026-08-09. This is the ground truth the phase plans start from; where the repo's own docs disagree with its Terraform, the Terraform (and the live boxes) win — the docs are stale by the owner's own account and get reset ([ADR 014](../decisions/014-scope-boundaries.md)).

## The repo

Terraform (migrating to OpenTofu, [ADR 002](../decisions/002-opentofu-s3-state.md)): two root modules — `llunde/` and `pyparser/` — sharing one `modules/hetzner-server` module, one Hetzner project, one API token. State is **local tfstate per root**, backed up by manually running `aws s3 cp` — the single most fragile thing here and the direct motivation for the S3 backend migration.

The shared module **adopts** already-running servers (`terraform import`): `prevent_destroy = true` plus `ignore_changes = [image, user_data, ssh_keys, keep_disk, backups]`. These are pets captured in code, not reproducible machines — the module cannot create a server from scratch, which is exactly what the NixOS direction ([ADR 001](../decisions/001-nixos-declarative-host.md)) replaces. All firewall rules are inbound TCP from `0.0.0.0/0` + `::/0`; outbound is unrestricted (cloudflared needs to dial out).

Also in the repo: `DOPPLER.md` (accurate and worth keeping in spirit), `CLOUDFLARE.md` docs per root, and two reference workflows `old.deploy-llunde.yml` / `old.deploy-pyparser.yml` from the old monorepo (analyzed below, then deleted in phase 1).

## The llunde box — one server, three tenants, a stale README

`llunde/server.tf` describes `ubuntu-llunde` (since renamed **llunde-cpx22**, becoming **llunde-01** per [ADR 003](../decisions/003-repo-shape.md)): cpx22, hel1, 46.62.214.182 / `2a01:4f9:c014:cbe0::/64`, Ubuntu 24.04 (kernel 6.8), 2 vCPU / 4 GB / 80 GB, €9.99/mo. It currently runs:

- the **old `/opt/llunde` docker-compose stack** (backend + nginx + rsynced frontend dist) — dies with the wipe;
- **openclaw**, an unrelated project (`/home/openclaw`) — owner accepted losing it (declarative in its own repo, nothing important; not redeployed by this repo, user slot reserved);
- an old `deploy` user (`/home/deploy`).

**Discovery — the README is wrong about ingress.** The repo README says the llunde box is "public nginx/certbot → inbound 22/80/443". The current `server.tf` says the opposite: *ingress is a Cloudflare Tunnel, inbound is SSH only*, and the firewall (`llunde-fw`) opens **port 22 only**. Consequences for phase 2:

1. The Hetzner firewall must gain 80/443 inbound rules before Caddy can serve anything ([ADR 006](../decisions/006-caddy-ingress.md), [ADR 012](../decisions/012-cloudflare-strategy.md) — DNS-only, no tunnel).
2. There is (presumably) a `cloudflared` tunnel serving llunde.no today; cutover must retire that tunnel and repoint DNS at the origin, not just swap processes on the box. Verify the tunnel's existence and the current DNS record type (tunnel CNAME vs A record) during phase-2 prep.
3. llunde.no already resolves to this machine's tunnel/IP either way — cutover remains an in-place event with a brief accepted downtime, no server move.

## The old deploy workflows (facts worth keeping, then deleted)

`old.deploy-llunde.yml`: frontend built in CI (pnpm/Vite; `VITE_TURNSTILE_SITE_KEY` pulled from Doppler config `ci` at build time) → rsync `dist/` to `/opt/llunde/frontend-dist`; backend image **plus a separate `migrate` image** → GHCR as `ghcr.io/fredrir/llunde-backend[-migrate]` (the new backend pushes to the same package name; old CI is dead, so no collision — but old `:latest` tags linger until first new push); deploy = ssh + `docker compose`, with the **host rendering `.env` from its own read-only Doppler `prd` service token** — a pattern the new design keeps via the Quadlet Doppler wrapper ([ADR 007](../decisions/007-sops-nix-bootstrap.md)). Legacy variable names say `LINODE_*`; the host is Hetzner. The `migrate` image is obsolete — the new backend runs Flyway at startup.

`old.deploy-pyparser.yml`: builds `ghcr.io/fredrir/pyparser-review` (Dockerfile target `review-serve`); `workflow_dispatch` accepts an existing `image_tag` — a manual pin/rollback mechanism worth preserving conceptually in the shared CI design ([ADR 010](../decisions/010-shared-ci-reusable-workflows.md)); deploy ships `docker-compose.prod.yml` + `deploy-remote.sh` to `/opt/pyparser/` and executes the script over SSH as user `leploy` (sic) — the script does backup, drain, migrate, restart, rollback-on-failure; host-managed `secrets.env` is never touched by CI; a best-effort GHCR prune keeps the 10 newest image versions (each a rollback target).

## pyparser — live production, do not disturb until phase 3

`pyparser/` root: server `llunde-parser`, **CCX23** (dedicated vCPU), hel1, ubuntu-24.04, firewall `pyparser-parser-fw` with inbound **22 only**; web traffic arrives via Cloudflare Tunnel to parser.llunde.no. The owner's "I can still SSH despite the tunnel" is therefore **not drift — the firewall deliberately allows public 22**; the tunnel only carries web ingress. The phase-3 audit question is whether to close 22 in favor of Tailscale ([ADR 008](../decisions/008-tailscale-management.md)), not to fix a misconfiguration. The root also manages the only remaining AWS footprint: it **references** (does not create) the existing S3 dataset bucket, adds public-access-block hardening, and IAM. Real data lives here (Postgres on the box + the S3 bucket) — hence phase 3's do-last, steady-state-`plan`-proof discipline.

## Doppler layout (from DOPPLER.md — accurate)

Two projects (`pyparser`, `llunde`), three configs each: `dev` (local), `prd` (prod runtime — the **host's** read-only token renders env), `ci` (deploy-only secrets — the **CI** token; a token reads only its own config, so the host token can never read deploy SSH keys). This separation survives into the new design; what changes is that CI stops holding SSH deploy keys at all (pull-based deploys, [ADR 010](../decisions/010-shared-ci-reusable-workflows.md)) and the host's Doppler token becomes a sops-nix bootstrap secret ([ADR 007](../decisions/007-sops-nix-bootstrap.md)).

## Cloudflare

Zone `llunde.no` is managed out-of-band (dashboard + per-root `CLOUDFLARE.md` notes) — two tunnels (llunde, pyparser) and the zone's records are invisible to code today. Bringing the zone under OpenTofu is a Should ([ADR 012](../decisions/012-cloudflare-strategy.md)); orange-cloud proxy/WAF is deferred to phase 4 with the trusted-proxy chain done end-to-end.
