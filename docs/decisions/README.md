# Decision records

Twenty ADRs: 001–014 from the 2026-08-09 design session, 015–016 from the round that followed, 017–019 from the edge/observability/GitOps re-evaluation, 020 replacing 019 two days later. They are commitments, not proposals — reopen one explicitly (edit its status, record why) rather than drifting from it in config.

An ADR records what was decided **and when**. Where reality moved on, the later ADR supersedes the earlier one and both stay: the superseded record is the reason the current shape looks the way it does.

| ADR | Decision | Status |
|---|---|---|
| [001](001-nixos-declarative-host.md) | NixOS as host OS + source of truth; nixos-anywhere + Disko; llunde-cpx22 wiped (openclaw included, accepted) | Accepted |
| [002](002-opentofu-s3-state.md) | OpenTofu; S3 remote state; the split roots merge into one flat root | Accepted |
| [003](003-repo-shape.md) | Flake at repo root; `hosts/` = diffs only via profiles; `services/` flat folders; `tofu/` subdir; llunde-01 naming | Accepted |
| [004](004-quadlet-own-abstraction.md) | Quadlet via own `mkQuadlet` Nix abstraction — literal unit text, zero third-party flakes | Accepted |
| [005](005-per-service-users-isolation.md) | Per-service rootless users; loopback-only cross-user; caps in units | Accepted |
| [006](006-caddy-ingress.md) | Caddy as sole public ingress/TLS, Quadlet under `edge` | Accepted · ingress path amended by [017](017-tunnel-ingress-llunde.md) |
| [007](007-sops-nix-bootstrap.md) | sops-nix for the bootstrap secrets; Doppler for the rest | Accepted |
| [008](008-tailscale-management.md) | Tailscale-only management; root over tailnet; staged port-22 closure | Accepted |
| [009](009-frontend-as-container.md) | Frontend as GHCR container behind Caddy — no rsync, symmetric with backend | Accepted |
| [010](010-shared-ci-reusable-workflows.md) | Shared reusable build-image workflow here; pull-based deploys; no SSH in CI | Accepted · host applies resolved by [020](020-gitops-pull-auto-apply.md) |
| [011](011-backups-restic-module.md) | restic → S3 via `modules/backups/` with per-service options | Accepted |
| [012](012-cloudflare-strategy.md) | Zone into tofu; edge strategy | Accepted · part 3 (orange-cloud proxy) superseded by [017](017-tunnel-ingress-llunde.md) |
| [013](013-observability-host-scope.md) | node_exporter + journald, tailnet-only scraping; collection deferred | Accepted · deferral resolved by [018](018-observability-collection.md) |
| [014](014-scope-boundaries.md) | Repo split; pyparser last and careful; openclaw not redeployed; k8s a non-goal | Accepted |
| [015](015-tailnet-only-management.md) | The tailnet covers CI too (ephemeral tagged keys); port 22 closes on both hosts | Accepted |
| [016](016-tenant-slots-host-uniformity.md) | Tenant slots declare user + deps only, tenants self-deploy; uniform NixOS hosts | Accepted |
| [017](017-tunnel-ingress-llunde.md) | llunde.no ingress via Cloudflare Tunnel; zero public inbound estate-wide | Accepted |
| [018](018-observability-collection.md) | Collection stack self-hosted on llunde-parser, tailnet-only, hard-capped | Accepted |
| [019](019-gitops-auto-apply.md) | Push auto-apply on merge, environment-scoped credentials | **Superseded** by [020](020-gitops-pull-auto-apply.md) — never built |
| [020](020-gitops-pull-auto-apply.md) | Pull auto-apply: hosts poll a gate-green `deploy` ref and apply themselves; no credential reaches CI | Accepted |
