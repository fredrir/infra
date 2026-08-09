# Decision records

Sixteen accepted ADRs — fourteen from the 2026-08-09 design session, two (015–016) from the post-phase-2 planning round the same day. They are commitments, not proposals — reopen one explicitly (edit status, record why) rather than drifting from it in config.

| ADR | Decision |
|---|---|
| [001](001-nixos-declarative-host.md) | NixOS as host OS + source of truth; nixos-anywhere + Disko; llunde-cpx22 wiped (openclaw included, accepted) |
| [002](002-opentofu-s3-state.md) | OpenTofu; S3 remote state per root; roots merge flat after phase 3 |
| [003](003-repo-shape.md) | Flake at repo root; hosts/ = diffs only via profiles; services/ flat folders; tofu/ subdir; llunde-01 naming |
| [004](004-quadlet-own-abstraction.md) | Quadlet via own `mkQuadlet` Nix abstraction — literal unit text, zero third-party flakes |
| [005](005-per-service-users-isolation.md) | Per-service rootless users (llunde-backend, llunde-frontend, edge); loopback-only cross-user; caps in units |
| [006](006-caddy-ingress.md) | Caddy as sole public ingress/TLS, Quadlet under `edge` |
| [007](007-sops-nix-bootstrap.md) | sops-nix for exactly three bootstrap secrets; Doppler for the rest |
| [008](008-tailscale-management.md) | Tailscale-only management; root over tailnet; staged port-22 closure |
| [009](009-frontend-as-container.md) | Frontend as GHCR container behind Caddy — no rsync, symmetric with backend |
| [010](010-shared-ci-reusable-workflows.md) | Shared reusable build-image workflow here; pull-based deploy; no ssh in CI |
| [011](011-backups-restic-module.md) | restic → S3 via modules/backups/ with per-service options; weekly to start |
| [012](012-cloudflare-strategy.md) | DNS-only now; zone into tofu as Should; proxy/WAF deliberately phase 4 |
| [013](013-observability-host-scope.md) | node_exporter + journald now, tailnet-only scraping; collection stack phase 4 |
| [014](014-scope-boundaries.md) | Repo split (rescopes backend phase-3); pyparser last+careful; openclaw not redeployed; k8s non-goal |
| [015](015-tailnet-only-management.md) | Tailnet covers CI too (ephemeral tagged keys); port 22 closes on both boxes in phase 3; SSH-in-CI is scaffolding until 3.5 |
| [016](016-tenant-slots-host-uniformity.md) | Tenant slots (`modules/tenants/` declares user+deps only, tenants self-deploy); uniform NixOS hosts as end state — phase 3.5 |
