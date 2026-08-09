# ADR 018: Observability Collection — Self-Hosted on llunde-parser

**Status**: Accepted · **Date**: 2026-08-09 (phase-4 re-evaluation) · Resolves the decision [ADR 013](013-observability-host-scope.md) deferred

## Context

ADR 013 shipped exporters and journald in phase 2 and deferred the collection stack because the only host was a 4 GB box. Phase 3.5 changed the estate: llunde-parser runs NixOS with 14 Gi of free memory and 131 G of free disk, and the estate's log-viewing story regressed deliberately (Dozzle retired) against a phase-4 promise. Nothing alerts today; metric history does not exist.

Measured sizing at this estate's scale (two hosts, ~15 containers, a few thousand series): Prometheus ~150–300 MB / 2–4 GB disk at 30–60 d retention; Grafana ~200 MB; Loki (single-binary, filesystem) ~300 MB / 2–10 GB bounded by retention; OTel collector ~150 MB; journal shippers ~100 MB per host. Total ≈ 1 GB RAM, ≤15 GB disk — under a tenth of the box.

## Decision

**The collection stack self-hosts on llunde-parser** as `services/observability/` quadlets under a dedicated user (ADR 005): Prometheus, Grafana, Loki, and an OTel collector; journal shippers on both hosts feed Loki over the tailnet; Prometheus scrapes both node_exporters and the backend's `/metrics` over the tailnet.

- **Tailnet-only, absolutely** (ADR 013's rule): Grafana binds the tailnet; no public surface, no Caddy route, no CF hostname.
- **Hard caps**: every unit carries `MemoryMax`; the stack totals ≤2 G so it can never squeeze the pyparser workers sharing the box.
- **Alerting minimums** (the original phase-4 list): `/ready` failures, disk usage, backup freshness (restic + portfolio's chain), failed units — delivered off-box (email via the zone's existing SES identity, or ntfy; chosen at authoring).
- **Retention bounded by config**, not hope: Prometheus 60 d, Loki 30 d to start.
- The OTel collector exists from day one; wiring the backend's Java agent to it is a backend-repo follow-up, not this phase.

## Alternatives considered

- **Grafana Cloud free tier** — no self-host maintenance, but ships estate telemetry to a vendor, adds an account to manage, and its free-tier limits bite exactly when observability matters (an incident). The memory constraint that made it attractive no longer exists. Rejected.
- **Alerts-only minimal probe** — cheapest, but defers the real decision a third time and leaves the Dozzle-replacement promise unpaid. Rejected.
- **Collection on llunde-01** — the 4 GB box shared with the actual product. Rejected on the same grounds as ADR 013.

## Consequences

- The estate gains metric history, cross-host log search (the Dozzle replacement, better), and its first alerting — the "is it healthy?" question stops requiring a laptop and SSH.
- llunde-parser becomes operationally load-bearing for observing llunde-01; if llunde-parser is down, so are the dashboards — accepted (alerts about llunde-parser itself must come from a path that does not require it, e.g. a dead-man's-switch style heartbeat, decided at authoring).
- Backups: dashboards/alert rules are declarative (provisioned from the repo); Prometheus/Loki data is deliberately NOT backed up — telemetry is rebuildable history, not data of record.
