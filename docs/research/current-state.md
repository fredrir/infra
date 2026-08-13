# Pre-migration snapshot — the repo and machines on 2026-08-09

> **Historical.** An audit of the transferred `llunde-infra` repo and the boxes
> it described, taken 2026-08-09 *before* the NixOS migration ran. Nothing here
> describes the estate today — see the [repo README](../../README.md) and
> [docs/runbook.md](../runbook.md). Kept because the migration was planned from
> it, and several ADRs cite it as their starting condition.

Where the audited repo's docs disagreed with its Terraform, the Terraform (and
the live boxes) won — the docs were stale by the owner's own account
([ADR 014](../decisions/014-scope-boundaries.md)). A companion snapshot of
`llunde-parser` and its tenants, taken the same day, is
[phase-3-mapping.md](phase-3-mapping.md); it found a second live production
(portfolio) on that box, which this audit did not cover.

## The repo

Terraform, migrating to OpenTofu
([ADR 002](../decisions/002-opentofu-s3-state.md)): two root modules — `llunde/`
and `pyparser/` — sharing one `modules/hetzner-server`, one Hetzner project, one
API token. State was **local tfstate per root**, backed up by manually running
`aws s3 cp` — the most fragile thing there, and the direct motivation for the S3
backend migration.

The shared module **adopted** running servers (`terraform import`):
`prevent_destroy = true` plus `ignore_changes = [image, user_data, ssh_keys,
keep_disk, backups]`. Pets captured in code — the module could not create a
server from scratch, which is what the NixOS direction
([ADR 001](../decisions/001-nixos-declarative-host.md)) replaced. All firewall
rules were inbound TCP from `0.0.0.0/0` + `::/0`; outbound unrestricted
(cloudflared needs to dial out).

Also present: `DOPPLER.md` (accurate), per-root `CLOUDFLARE.md` docs, and two
reference workflows preserved in [old-workflows/](old-workflows/).

## The llunde box — one server, three tenants, a stale README

`llunde/server.tf` described `ubuntu-llunde` (since renamed **llunde-cpx22**,
then **llunde-01** per [ADR 003](../decisions/003-repo-shape.md)): cpx22, hel1,
46.62.214.182 / `2a01:4f9:c014:cbe0::/64`, Ubuntu 24.04 (kernel 6.8),
2 vCPU / 4 GB / 80 GB, €9.99/mo. It ran the old `/opt/llunde` docker-compose
stack (backend + nginx + rsynced frontend dist), **openclaw** in
`/home/openclaw` — an unrelated project the owner accepted losing, declarative in
its own repo, not redeployed here though its user slot is reserved — and an old
`deploy` user.

**The README was wrong about ingress.** It claimed "public nginx/certbot →
inbound 22/80/443"; `server.tf` said the opposite — *ingress is a Cloudflare
Tunnel, inbound is SSH only* — and the `llunde-fw` firewall opened **port 22
only**. So the migration had to: add 80/443 inbound before Caddy could serve
anything ([ADR 006](../decisions/006-caddy-ingress.md),
[ADR 012](../decisions/012-cloudflare-strategy.md)); find and retire the
presumed `cloudflared` tunnel serving llunde.no rather than just swapping
processes, verifying first whether the record was a tunnel CNAME or an A record;
and treat cutover as an in-place event with brief accepted downtime, since
llunde.no already resolved to this machine either way.

## The old deploy workflows

`old.deploy-llunde.yml`: frontend built in CI (pnpm/Vite,
`VITE_TURNSTILE_SITE_KEY` from Doppler `ci` at build time) → rsync `dist/` to
`/opt/llunde/frontend-dist`; backend image **plus a separate `migrate` image** →
GHCR as `ghcr.io/fredrir/llunde-backend[-migrate]` (the new backend pushes to
the same package name — no collision since the old CI is dead, but old `:latest`
tags linger until the first new push); deploy = ssh + `docker compose`, with the
**host rendering `.env` from its own read-only Doppler `prd` token** — a pattern
the new design kept ([ADR 007](../decisions/007-sops-nix-bootstrap.md)). Legacy
variable names say `LINODE_*`; the host is Hetzner. The `migrate` image is
obsolete — the new backend runs Flyway at startup.

`old.deploy-pyparser.yml`: built `ghcr.io/fredrir/pyparser-review` (Dockerfile
target `review-serve`); `workflow_dispatch` accepted an existing `image_tag`, a
manual pin/rollback lever worth preserving conceptually
([ADR 010](../decisions/010-shared-ci-reusable-workflows.md)); deploy shipped
`docker-compose.prod.yml` + `deploy-remote.sh` to `/opt/pyparser/` and ran the
script over SSH as user `leploy` (sic) — backup, drain, migrate, restart,
rollback-on-failure; host-managed `secrets.env` never touched by CI; a
best-effort GHCR prune kept the 10 newest image versions, each a rollback target.

## pyparser — live production at the time of the audit

`pyparser/` root: server `llunde-parser`, **CCX23** (dedicated vCPU), hel1,
ubuntu-24.04, firewall `pyparser-parser-fw` inbound **22 only**; web traffic via
Cloudflare Tunnel to parser.llunde.no. The owner's "I can still SSH despite the
tunnel" was therefore **not drift — the firewall deliberately allowed public
22**; the tunnel carried only web ingress. The open question was whether to close
22 in favour of Tailscale ([ADR 008](../decisions/008-tailscale-management.md)),
not to fix a misconfiguration. The root also held the only remaining AWS
footprint: it **references** (does not create) the existing S3 dataset bucket,
adds public-access-block hardening, and IAM. Real data lived here — Postgres on
the box plus the S3 bucket — hence the do-last, steady-state-`plan`-proof
discipline applied to it.

## Doppler layout (from DOPPLER.md — accurate)

Two projects (`pyparser`, `llunde`), three configs each: `dev` (local), `prd`
(prod runtime, rendered by the **host's** read-only token), `ci` (deploy-only
secrets, the **CI** token — a token reads only its own config, so the host token
could never read deploy SSH keys). The separation survived; what changed is that
CI stopped holding SSH deploy keys at all
([ADR 010](../decisions/010-shared-ci-reusable-workflows.md)) and the host's
Doppler token became a sops-nix bootstrap secret
([ADR 007](../decisions/007-sops-nix-bootstrap.md)).

## Cloudflare

Zone `llunde.no` was managed out-of-band (dashboard + per-root `CLOUDFLARE.md`
notes) — two tunnels (llunde, pyparser) and the zone's records were invisible to
code. Bringing the zone under OpenTofu was a Should
([ADR 012](../decisions/012-cloudflare-strategy.md)); proxy/WAF was deferred, and
[ADR 017](../decisions/017-tunnel-ingress-llunde.md) later replaced that plan
with tunnel ingress for llunde.no as well.
