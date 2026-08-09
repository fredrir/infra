# Phase 4 — tunnel edge, observability collection, GitOps auto-apply (full plan)

**Re-evaluated and decided 2026-08-09**, after the phase-3.5 gate closed. The original top-level sketch assumed one memory-constrained host, no tunnel operating experience, and no CI-to-tailnet pattern — all three constraints died with phases 3/3.5. The owner decided the three items in the re-evaluation round:

1. **llunde.no's edge moves to a Cloudflare Tunnel** ([ADR 017](../../../decisions/017-tunnel-ingress-llunde.md)) — the owner's original instinct ("tunnels for all services"), now backed by months of in-estate tunnel production. llunde-01 reaches **zero public inbound ports**, matching llunde-parser: full estate uniformity (ADR 016's principle applied to ingress).
2. **Observability collection self-hosts on llunde-parser** ([ADR 018](../../../decisions/018-observability-collection.md)) — Prometheus + Grafana + Loki + OTel collector as quadlets on the 14 Gi-free box, tailnet-only, ~1 GB RAM / ~5–15 GB disk, hard-capped.
3. **GitOps goes to full auto-apply on merge** ([ADR 019](../../../decisions/019-gitops-auto-apply.md)) — owner's call, past the lead's eval-gate-only recommendation; the ADR carries the guardrails that make it acceptable.

**Reads first**: ADRs 017/018/019 (new), 012/013/010 (amended/superseded in part), [tasks.md](tasks.md).

## Execution order (re-evaluated)

**Observability → edge cutover → GitOps.** Observability first because it has immediate value (nothing alerts today; metric history doesn't exist; the Dozzle replacement was promised) and zero risk. The edge cutover second, in its own small window with the real-IP verification. GitOps last — auto-apply lands when the estate is quietest, and its gate job alone (flake check + closure builds in CI, zero credentials) lands FIRST, before everything, because nothing currently stops a broken flake from reaching main.

## In scope

- **Workstream G — CI gate (first, tiny)**: llunde-infra workflow on PR + push: `nix flake check` + build both host closures (x86_64-linux runner). No credentials. Required check on main.
- **Workstream O — observability** (`services/observability/` on llunde-parser, new `observability` user per ADR 005): Prometheus (scraping both hosts' node_exporters + the backend's `/metrics` over the tailnet), Loki + per-host journal shippers (Alloy/promtail as quadlet on llunde-parser, NixOS-native shipper on llunde-01), Grafana bound tailnet-only, OTel collector (tracing wiring in the backend is a follow-up, not this phase). Alert rules: `/ready` failures, disk >80 %, restic/backup freshness, unit-failed — delivery via email (SES — the zone's DKIM/SPF already exist) or ntfy, decided at authoring. MemoryMax on every unit; stack total ≤2 G.
- **Workstream E — edge tunnel** ([ADR 017](../../../decisions/017-tunnel-ingress-llunde.md)): a new named tunnel `llunde` (created via dashboard/API, token into sops as a llunde-01 secret), `cloudflared` quadlet under `edge` dialing Caddy on loopback; Caddy vhosts stay (routing + ops-endpoint blocking) but drop LE for the tunneled hosts; **real-IP contract**: Caddy maps `CF-Connecting-IP` into the XFF chain and the backend's rotating-XFF test verifies buckets cannot be minted, end-to-end, BEFORE DNS moves; DNS flip in tofu (A/AAAA → proxied tunnel CNAMEs — a reviewed diff, ADR 012's promise); then 80/443 close in tofu + NixOS firewall → **zero public inbound estate-wide**. Break-glass documented: grey-flip + firewall reopen, both tofu diffs.
- **Workstream A — auto-apply** ([ADR 019](../../../decisions/019-gitops-auto-apply.md)): after the gate passes on a main push, a tailnet job (`tag:ci` ephemeral key) runs `nixos-rebuild switch --flake .#<host> --target-host --build-host` sequentially per host. Credentials (deploy SSH key + tailnet key) live in a GitHub **environment**; the manual laptop path stays the documented recovery route; failure alerts through workstream O.

## Out of scope

Backend tracing wiring (backend repo follow-up once the collector exists). pyparser/portfolio edges (already tunneled; untouched). `parser.llunde.no/logs`-style public log UI (Grafana stays tailnet-only; revisit only if the owner misses it). Multi-host deploy tooling (colmena etc.) — still two hosts, still unnecessary.

## Hard rules

1. Workstream E's DNS flip happens only after the real-IP chain is verified through a test hostname on the new tunnel; `llunde.no` availability is the gate metric; the grey-flip break-glass is rehearsed as a written procedure before cutover.
2. Workstream O adds zero public surface — every UI and scrape path is tailnet-only.
3. Workstream A merges only after G has been a required check long enough to have caught at least one real eval failure or one full week, whichever first; applies are sequential and stop on first failure.

## Exit criteria (gate)

1. CI gate red-blocks a deliberately broken flake on a test PR; green on main.
2. Grafana (tailnet) shows live dashboards for both hosts + the backend; Loki answers a cross-host journal query; one alert fires end-to-end on a forced condition (e.g. stopped unit) and reaches the owner.
3. `llunde.no`/`www`/`api` serve through the tunnel with the XFF/rate-limit verification green; `ss` and the Hetzner firewall show **zero public inbound on both hosts**; origin A/AAAA records gone from DNS; break-glass procedure written and its tofu diffs pre-staged.
4. A trivial infra merge applies itself to both hosts with zero laptop involvement; a deliberately failing apply stops the sequence and alerts; the manual path demonstrated still working afterward.
5. tofu plan clean; flake goldens extended where load-bearing (cloudflared unit, observability units).
6. Owner review → gate closed.
