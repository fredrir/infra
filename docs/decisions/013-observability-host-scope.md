# ADR 013: Observability — Exporters Now, Collection Stack Later

**Status**: Accepted · **Date**: 2026-08-09

## Context

The application side is already done: the backend ships JSON logs to stdout, a Prometheus `/metrics` endpoint with app/environment tags, khealth `/health` + `/ready`, and trace-readiness with the collector deliberately deferred (backend ADR 010). The owner wants "logging, metrics, tracing, collection, health check etc." included in the infra design — the open question was how much of the *collection* side to stand up now on a 4 GB box shared with everything else.

A separate constraint carries over from the backend's phase-1 gate review: `/metrics`, `/health`, `/ready` are unauthenticated and must never be publicly reachable.

## Decision

**Phase 2 ships the minimal, load-bearing layer as `modules/observability/`; the collection stack is a phase-4 decision.**

Now (phase 2):

- **node_exporter** on every host, bound to the tailnet interface.
- **journald as the log spine**: Quadlet containers' JSON stdout lands in the journal automatically — structured logs are queryable on-box (`journalctl -u llunde-backend -o json`) with zero extra services.
- **Scrape surface is tailnet-only** ([ADR 008](008-tailscale-management.md)): app `/metrics` and node metrics reachable over Tailscale, never via Caddy, never on public interfaces — satisfying the backend's proxy-blocking requirement by never exposing them in the first place.
- Health checks: Caddy and the Quadlet units use `/ready` for their own wiring; external uptime monitoring (if any) hits public endpoints only.

Later (phase 4): the collection decision — self-hosted Prometheus/Grafana/Loki vs Grafana Cloud free tier, plus the OTel collector that unlocks the backend's deferred tracing. Declared out of scope now because it deserves its own sizing/cost decision and the box is memory-constrained.

## Alternatives considered

- **Full self-hosted stack now** (Prometheus + Grafana + Loki + collector) — 1–2 GB of the 4 GB box for dashboards nobody looks at pre-users. Rejected for now; the module structure makes it additive later.
- **Grafana Cloud now** — plausible, but it forces the vendor decision before there's traffic to inform it. Deferred to phase 4 with the rest.
- **Nothing until it hurts** — rejected: exporters and journald cost almost nothing and make the box diagnosable from day one; retrofitting them mid-incident is the worst time.

## Consequences

- The box is observable the day it boots: metrics scrapeable from the laptop over the tailnet, structured logs in the journal, no public surface added.
- Metric history does not exist until phase 4 (nothing retains scrapes) — accepted; incident diagnosis pre-phase-4 is live-scrape plus journal.
- `modules/observability/` is the single place phase 4 extends; no rework, only addition.
- The backend's `-Dlogback.configurationFile=logback-prod.xml` JSON-logging flag becomes part of the backend's Quadlet unit definition here — the flag the backend's review warned would otherwise be silently forgotten.
