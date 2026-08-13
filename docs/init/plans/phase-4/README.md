# Phase 4 — tunnel edge, observability collection, GitOps auto-apply (full plan)

**Re-evaluated and decided 2026-08-09**, after the phase-3.5 gate closed. The original top-level sketch assumed one memory-constrained host, no tunnel operating experience, and no CI-to-tailnet pattern — all three constraints died with phases 3/3.5. The owner decided the three items in the re-evaluation round:

1. **llunde.no's edge moves to a Cloudflare Tunnel** ([ADR 017](../../../decisions/017-tunnel-ingress-llunde.md)) — the owner's original instinct ("tunnels for all services"), now backed by months of in-estate tunnel production. llunde-01 reaches **zero public inbound ports**, matching llunde-parser: full estate uniformity (ADR 016's principle applied to ingress).
2. **Observability collection self-hosts on llunde-parser** ([ADR 018](../../../decisions/018-observability-collection.md)) — Prometheus + Grafana + Loki + OTel collector as quadlets on the 14 Gi-free box, tailnet-only, ~1 GB RAM / ~5–15 GB disk, hard-capped.
3. **GitOps goes to full auto-apply on merge** ([ADR 019](../../../decisions/019-gitops-auto-apply.md)) — owner's call, past the lead's eval-gate-only recommendation; the ADR carries the guardrails that make it acceptable.

**Reads first**: ADRs 017/018/019 (new), 012/013/010 (amended/superseded in part), [tasks.md](tasks.md).

## Execution order (re-evaluated)

**Observability → edge cutover → GitOps.** Observability first because it has immediate value (nothing alerts today; metric history doesn't exist; the Dozzle replacement was promised) and zero risk. The edge cutover second, in its own small window with the real-IP verification. GitOps last — auto-apply lands when the estate is quietest, and its gate job alone (flake check + closure builds in CI, zero credentials) lands FIRST, before everything, because nothing currently stops a broken flake from reaching main.

## In scope

- **Workstream G — CI gate (first, tiny)**: **replaces** the stale red `ci.yml` (it validates tofu roots deleted in phase 3's unnesting): `nix flake check` + both host closures + tofu fmt/validate, SHA-pinned actions, no credentials. Required check on main.
- **Workstream O — observability** (`services/observability/` on llunde-parser, new `observability` user per ADR 005): Prometheus (both node_exporters + the backend's `/metrics` via a Caddy tailnet listener), Loki + `services.promtail` on both hosts, Grafana bound tailnet-only, OTel collector (backend tracing wiring is a follow-up), **a blackbox probe of the public URLs through the CF edge, and an external gate-tested dead-man** (second-opinion additions). Alert rules: `/ready` failures, disk >80 %, restic/backup freshness, unit-failed — delivery default SES. MemoryMax on every unit; stack total ≤2 G.
- **Workstream E — edge tunnel** ([ADR 017](../../../decisions/017-tunnel-ingress-llunde.md)): a new named tunnel `llunde` (created via dashboard/API, token into sops as a llunde-01 secret), `cloudflared` quadlet under `edge` dialing Caddy on loopback; Caddy vhosts stay (routing + ops-endpoint blocking) but drop LE for the tunneled hosts; **real-IP contract**: Caddy maps `CF-Connecting-IP` into the XFF chain and the backend's rotating-XFF test verifies buckets cannot be minted, end-to-end, BEFORE DNS moves; DNS flip in tofu (A/AAAA → proxied tunnel CNAMEs — a reviewed diff, ADR 012's promise); then 80/443 close in tofu + NixOS firewall → **zero public inbound estate-wide**. Break-glass documented: grey-flip + firewall reopen, both tofu diffs.
- **Workstream A — auto-apply** ([ADR 019](../../../decisions/019-gitops-auto-apply.md)): after the gate passes on a main push, a tailnet job (`tag:ci` ephemeral key) runs `nixos-rebuild switch --flake .#<host> --target-host --build-host` sequentially per host. Credentials (deploy SSH key + tailnet key) live in a GitHub **environment** restricted to main; actions SHA-pinned; applies serialized; **failure notifies from the workflow itself** (the observability stack may be exactly what a failed apply broke); the manual laptop path stays the documented recovery route.

## Out of scope

Backend tracing wiring (backend repo follow-up once the collector exists). pyparser/portfolio edges (already tunneled; untouched). `parser.llunde.no/logs`-style public log UI (Grafana stays tailnet-only; revisit only if the owner misses it). Multi-host deploy tooling (colmena etc.) — still two hosts, still unnecessary.

## Hard rules

1. Workstream E's DNS flip happens only after the real-IP chain is verified through a test hostname on the new tunnel; `llunde.no` availability is the gate metric; the grey-flip break-glass is rehearsed as a written procedure before cutover.
2. Workstream O adds zero public surface — every UI and scrape path is tailnet-only.
3. Workstream A merges only after G has been a required check long enough to have caught at least one real eval failure or one full week, whichever first; applies are sequential and stop on first failure.

## Exit criteria (gate)

> **✅ Gate CLOSED 2026-08-13 on `f82ae3a`.** All six walked with live evidence in
> [tasks.md § Executed](tasks.md#executed--gate-closed-2026-08-13), including the
> two criteria that closed with a stated deviation rather than cleanly: #1 (the
> gate cannot be a *required* check on a private repo) and #2 (O5 landed 3 of its
> 5 alerts; `unit-failed` and portfolio-backup freshness are backlog H4). #4 was
> met under ADR 020's pull design, which superseded the push design this
> criterion was written against.

1. CI gate red-blocks a deliberately broken flake on a test PR; green on main.
2. Grafana (tailnet) shows live dashboards for both hosts + the backend; Loki answers a cross-host journal query; one alert fires end-to-end on a forced condition (e.g. stopped unit) and reaches the owner; **the public-path blackbox probe is green through the CF edge; stopping the observability stack notifies the owner within 15 minutes via the external dead-man** (second-opinion additions — the alerting-is-dark case and the public path were previously unwatched).
3. `llunde.no`/`www`/`api` serve through the tunnel with the XFF/rate-limit verification green; `ss` and the Hetzner firewall show **zero public inbound on both hosts**; origin A/AAAA records gone from DNS; break-glass procedure written and its tofu diffs pre-staged.
4. A trivial infra merge applies itself to both hosts with zero laptop involvement; a deliberately failing apply stops the sequence and alerts; the manual path demonstrated still working afterward.
5. tofu plan clean; flake goldens extended where load-bearing (cloudflared unit, observability units).
6. Owner review → gate closed.
