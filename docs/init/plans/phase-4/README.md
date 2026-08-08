# Phase 4 — GitOps, Cloudflare edge, observability collection (top-level plan)

The deliberately deferred trio. Each item was scoped out of earlier phases on purpose; each gets its detailed plan only when it starts, informed by how phases 1–3 actually went.

## 1. GitOps CI-apply ([ADR 010](../../../decisions/010-shared-ci-reusable-workflows.md))

Merge-to-main applies the NixOS configuration — a tailnet-connected runner (or ephemeral runner joining via auth key) runs `nixos-rebuild switch --flake .#<host> --target-host` for changed hosts. Until then, applies stay manual from the laptop (`just`-wrapped `nixos-rebuild --target-host` over Tailscale). Multi-host tooling (colmena/deploy-rs) becomes worth evaluating here, once pyparser makes it two hosts.

Design constraints already settled: CI never holds broader credentials than "rebuild these hosts"; rollback story is NixOS generations; the manual path must keep working (CI is a convenience, not a gatekeeper — same principle as the backend's justfile rule).

## 2. Cloudflare proxy / WAF ([ADR 012](../../../decisions/012-cloudflare-strategy.md))

Orange-cloud `llunde.no` + `api.llunde.no` for WAF/bot-filtering/DDoS. **The known trap is real-IP handling**: Caddy must trust Cloudflare's ranges (`trusted_proxies`) and forward true client IPs, or the backend's per-IP rate limiting and audit trail silently key on CF datacenter addresses — the exact bug class the backend's external review caught. The phase must verify the CF → Caddy → app chain end-to-end, including the backend's rotating-XFF behavior, before the proxy goes live. TLS mode + origin certificates decided here too.

## 3. Observability collection ([ADR 013](../../../decisions/013-observability-host-scope.md))

Everything is already emitting (app JSON logs + `/metrics` + trace-ready; host node_exporter + journald, tailnet-only). This phase decides and builds collection: self-hosted Prometheus/Grafana/Loki (weigh the 4 GB box; likely rescale or a second small box) vs Grafana Cloud free tier; OTel collector + the backend's Java agent for tracing (backend ADR 010 planned exactly this). Alerting minimums: `/ready` failures, disk, backup-freshness.

## Ordering

No fixed order among the three; each is independently valuable. Recommended default: **2 → 3 → 1** (edge protection before public growth; dashboards before automation; automation last when host count and cadence justify it).
