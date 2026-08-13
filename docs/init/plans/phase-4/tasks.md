# Phase 4 — task breakdown

House rules as ever: declarative files in parallel; live surfaces (DNS, firewalls, the boxes, tailnet ACLs, GitHub environments) serial by the lead; `nix flake check` green at every step; deviations fix the doc on the spot. Gate metrics throughout: `llunde.no`, `parser.llunde.no`, `hansteen.dev`.

## Workstream G — CI gate (first, before everything)

| # | Task | Proof | Rollback |
|---|---|---|---|
| G1 | **Replace** the stale `.github/workflows/ci.yml` (second-opinion finding: it validates tofu roots deleted in the unnesting — permanently red and ignored, alarm-fatigue conditions) with `check.yml`: on PR + push-to-main, `nix flake check`, build BOTH host closures, `tofu fmt -check` + `tofu validate` on the flat root. All actions **SHA-pinned**. No credentials | green run on main; old red job gone | restore ci.yml |
| G2 | Make it a required check on `main`; prove it red-blocks a deliberately broken flake on a test PR. **As executed**: GitHub requires Pro for branch-protection *enforcement* on private repos (403) — the gate runs on every PR/push and went properly red on the broken-flake test (PR #9), and the apply workflow depends on it **structurally** via `needs:`; the only unenforceable part is "cannot click merge on red", advisory for a solo owner. Revisit if the repo ever goes Pro/public | red `flake` check on PR #9; green on main | delete workflow |

## Workstream O — observability (parallel authoring; lead applies)

| # | Task | Proof | Rollback |
|---|---|---|---|
| O1 | Contract mini-freeze: user `observability` uid 2002 on llunde-parser (20xx block is per-host; reuse across hosts is intended — 2001 already means different services per host), unit list (prometheus, grafana, loki, otel-collector, promtail), tailnet bind addresses/ports, retention (prom 60 d, loki 30 d), `MemoryMax` table (stack ≤2 G), alert delivery (default SES — the zone's DKIM exists), **and the backend-scrape mechanism (review finding: `/metrics` binds 127.0.0.1 on llunde-01 and was never reachable off-box)**: Caddy (already host-network, already reaches the backend loopback) gains a tailnet-bound listener proxying ONLY `/metrics` | contract section committed in this file's header | — |
| O2 | `services/observability/` quadlets + provisioning-as-code (Grafana datasources/dashboards/alert rules from the repo, not clicked) | flake builds; golden for the scrape config | revert |
| O3 | Journal shippers: `services.promtail` (NixOS-native, journal scrape → Loki push over the tailnet, ~100 MB cap) on BOTH hosts — one module, two hosts, no quadlet needed | logs from BOTH hosts answer one Loki query | disable shipper units |
| O4 | Scrapes: both node_exporters + backend `/metrics` via the O1 Caddy tailnet listener; dashboards: host essentials + backend JVM/HTTP + pyparser unit health | live panels for both hosts | — |
| O5 | Alerts wired: `/ready` fail, disk >80 %, restic freshness, portfolio backup freshness, unit-failed | force one condition (stop a unit) → alert reaches the owner | silence rules |
| O6 | **Public-path probe** (second-opinion insisted): blackbox-exporter on llunde-parser probing `https://llunde.no` + `https://api.llunde.no/<public path>` — egress to the CF edge, back through the tunnel, exercising the WHOLE public chain the tailnet-side checks can't see (dead route, revoked token, CF incident) | probe panel green; alert on probe failure | disable probe |
| O7 | **External dead-man, named and gate-tested** (second-opinion insisted): the alert pipeline pings healthchecks.io (or equivalent free service) on a schedule; a missed ping notifies the owner from OUTSIDE the estate — covering "llunde-parser (and thus all alerting) is dark" and disambiguating a tailscale outage from a host-down | stop the observability stack → owner notified within 15 min via the external service | delete check |

## Workstream E — edge tunnel (serial where live)

| # | Task | Proof | Rollback |
|---|---|---|---|
| E1 | Create named tunnel `llunde` (dashboard/API); token → new sops secret on llunde-01; ingress config (remote-managed): `llunde.no`→caddy, `www`→caddy, `api`→caddy, all `http://localhost:<caddy-port>`; **plus the E3 test hostname with `httpHostHeader: api.llunde.no`** (review finding: Caddy routes by Host — without the override the test hostname matches no vhost and the proof cannot run) | tunnel healthy, no DNS yet | delete tunnel |
| E2 | `cloudflared` quadlet under `edge`, **image pinned by digest + `MemoryMax`** (second-opinion finding: pyparser's `:latest` cloudflared precedent must NOT extend to the front door — a broken upstream release would take the site down; updates are deliberate digest bumps), **`Network=host`** (it dials Caddy on loopback — a podman-networked container's localhost is its own; pyparser's shared-network shape does NOT transfer); Caddy gains a loopback plain-HTTP listener for tunnel traffic which must (review findings, all three load-bearing): **(a) set `X-Forwarded-Proto=https`** (plain-HTTP hop otherwise reports http → Secure cookies, CSRF origin checks and redirects break), **(b) make `CF-Connecting-IP` the rightmost XFF client entry, scoped to THIS listener only** (unscoped, the public-443 path that coexists until E6 lets attackers supply the header; unmapped, every rate-limit bucket keys on 127.0.0.1 — cloudflared's loopback address), **(c) carry the same `@ops` 403 blocks as the public vhosts** (or /metrics goes public through the tunnel); **ACME choice, explicit** (second-opinion finding): the tunneled vhosts DROP active ACME (continuous failed renewals against closed 80/443 otherwise) — certs go stale, and break-glass consciously pays the documented TLS-outage window; the DNS-01-via-CF-API alternative (warm certs, zero outage, custom Caddy build) was considered and declined as complexity the estate doesn't need yet | units green; flake golden for the cloudflared unit incl. the three listener properties | disable unit |
| E3 | **Real-IP + proto verification on the test hostname** (Host-overridden to the real api vhost, per E1): the backend's rotating-XFF proof end-to-end — spoofed XFF AND spoofed `CF-Connecting-IP` from the public edge must NOT mint rate-limit buckets; audit records show true client IPs; **assert Secure-cookie issuance and CSRF acceptance work through the edge** (the X-Forwarded-Proto property) | test transcript in the PR/commit | remove test hostname |
| E4 | Break-glass rehearsal ON PAPER + pre-staged: the two tofu diffs (grey A/AAAA restore; 80/443 reopen) committed as documented patches in the runbook — **including the expected TLS-outage window** (review finding: after grey-flip, Caddy must re-issue LE certs only after DNS propagates AND 80/443 reopen; HSTS makes the interim errors non-bypassable — minutes of hard failure are break-glass WORKING, not broken) | runbook section reviewed | — |
| E5 | 💥 DNS flip in tofu: A/AAAA → proxied tunnel CNAMEs; verify `llunde.no`/`www`/`api` through the edge (full auth lifecycle spot-check against api — cookies + CSRF through CF edge TLS); **re-verify `/metrics`/`/health`/`/ready` 403 through the edge** (E2c) — probe the SLASH AND ENCODED VARIANTS, not just the bare paths: `curl --path-as-is` over `/metrics`, `/metrics/`, `/metrics//`, `/metrics%2f`, `/METRICS`, `/metrics/x` (and the same for health/ready), since a bare `path` matcher is exact and lets `/metrics/` through to the app; watch one auto-update cycle | all 200/expected; real IPs in audit; **every ops variant 403, asserted on the 403 itself — a 404 means the request reached the backend, not that it was blocked** | pre-staged grey-flip diffs |
| E6 | Close 80/443: tofu firewall + NixOS `publicTCPPorts = []` on llunde-01 → estate-wide zero public inbound; `ss` + timeout probes both boxes | probes time out; sites up | reopen diffs (pre-staged) |

## Workstream A — auto-apply (last; only after G has run ≥1 week or caught a real failure)

| # | Task | Proof | Rollback |
|---|---|---|---|
| A1 | Mint the deploy key (ed25519, single-purpose) + authorize for root on both hosts (declarative: `users.users.root.openssh.authorizedKeys` gains the key — **this first authorization necessarily applies via the manual path**: the workflow cannot bootstrap its own credential); mint `tag:ci` tailnet key; both into GitHub **environment** `infra-apply` | keys nowhere else | remove authorized_keys line + revoke tailnet key |
| A2 | `apply.yml`: needs gate-success, main only, environment `infra-apply` **restricted to the main branch from day one** (+ optional short wait-timer as an abort window); join tailnet; sequential `nixos-rebuild switch --flake .#llunde-01 …` then `.#llunde-parser`, `--build-host` = target; fail-fast; **`concurrency` group serialized, `cancel-in-progress: false`** (two quick merges must not race switches on one host); **every action SHA-pinned** (this workflow holds root — the unpinned-@main habit is the most plausible compromise path); **failure notifies from the WORKFLOW itself** (ntfy/email step — the observability stack may be exactly what the failed apply broke); **path-guard with teeth**: changes under `modules/tailscale/`, `modules/profiles/server.nix`, or firewall-touching tofu fail the apply job with a manual-path pointer (ADR 019 guardrail 4 enforced, not just stated) | dry: workflow lints; live: A3 | disable workflow |
| A3 | Prove: trivial merge (comment change) applies to both hosts with zero laptop involvement; then a deliberately failing change on a branch → gate blocks it; then a change that evals but fails at switch (e.g. bad unit) → apply stops, alert fires (workstream O), manual rollback demonstrated | three transcripts | revert commits |
| A4 | Runbook: applies-pause procedure (disable environment), the manual path re-affirmed as recovery, ADR 019 cross-linked; note the unattended eval/build load on the 4 GB box (proven interactively since phase 2 — if it ever bites, build llunde-01's closure on llunde-parser or substitute from the gate) | doc review | — |

## Gate

[README](README.md) exit criteria walked top-to-bottom; owner review closes the phase.

## Executed — gate CLOSED 2026-08-13

Walked top-to-bottom on `f82ae3a`, with `main` = `deploy` = `applied` on both
hosts and zero failed units. Every claim below was checked against the running
estate, not against the plan. **Phase 4 is complete.**

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Gate red-blocks a broken flake; green on main | ✅ *with a stated limit* | PR #9 went properly red and was closed unmerged; green `Check` on every merge since. **Not enforceable as a required check** — GitHub gates branch protection on private repos behind Pro (403). `promote.yml` depends on it structurally (`workflow_run` + `conclusion == 'success'`), so a red gate still means no deploy; only "cannot click merge" is advisory. Revisit if the repo goes Pro/public |
| 2 | Grafana live, Loki cross-host, alert end-to-end, blackbox green, dead-man ≤15 min | ✅ *3 of 5 contract alerts* | 7 scrape targets up, 4 probes `probe_success = 1`, promtail shipping from both hosts. `observability-deadman.timer` pings a real healthchecks.io URL every 5 min. **Carve-out below** |
| 3 | Tunnel serving, XFF verified, zero public inbound, origin records gone, break-glass staged | ✅ | ADR 017 §Executed. Re-verified at closeout: `llunde.no` 200 + `CF-Ray`, `api/health` 403, all eight `@ops` traversal variants 403, origin `:80` **and** `:443` exit 28, `nixos-fw` carries no 80/443 rule in either family, no name in the zone resolves to a host address |
| 4 | Trivial merge applies itself to both hosts; failing apply stops + alerts; manual path still works | ✅ *under ADR 020's shape* | The criterion describes ADR 019's **push** design, which ADR 020 superseded with **pull** before it was built. Pull equivalents all proven: rehearsed on a throwaway box (happy path, build-fail, two deadman rollbacks, hold semantics, `hcloud reset`), then live — this closeout itself rode the loop to both hosts untouched. The failure path was exercised **in production**, not only in rehearsal: H1b's stalled reconcile went sticky and fail-pinged exactly as designed |
| 5 | tofu plan clean; goldens extended where load-bearing | ✅ | `tofu plan` → *"No changes."* Goldens now cover the Caddyfile, caddy unit, cloudflared unit, valkey, grafana and the prometheus unit + scrape config; blackbox/prometheus/alerting configs carry syntax gates |
| 6 | Owner review | ✅ **closed 2026-08-13** | This block |

### Deviations, recorded rather than smoothed over

- **O5 landed 3 of its 5 alerts.** Live: `InstanceDown`, `ReadyProbeFailed`,
  `PublicEdgeDown`, `DiskHigh`, `ResticStale` — five rules, but not the five O5
  names. **`unit-failed` (both hosts) and portfolio-backup freshness were not
  built** and are parked as [backlog](../../../backlog.md) H4. The data for
  unit-failed is already being collected (node_exporter runs with
  `enabledCollectors = ["systemd"]` on both hosts), so it is a rule, not a
  project — and under auto-apply it is arguably the most load-bearing of the
  five, since reconcile failures deliberately never reboot. Criterion 2 is
  closed **with that carve-out explicit**, not with the gap implied away.
- **A forward `git push <sha>:deploy`** moved the estate by hand on 2026-08-12
  when a GitHub release-CDN outage wedged the `tofu` job and froze all deploys.
  Bypasses ADR 020's gate-green invariant; justified by a tree byte-identical to
  a gate-green PR head, no tofu in the diff, and the gate re-run locally on the
  merged commit. It stayed structurally safe because a `<sha>:deploy` push is a
  fast-forward, so `promote.yml`'s FF-only invariant was never violated and the
  next green promote simply continued from it. Runbook §9; root cause fixed
  (provider cache) plus a backlog item (providers via nix).
- **A stalled reconcile was hand-recovered** during H1b: the check ran from the
  old generation and graded the new Caddyfile with a plugin-less caddy. Fail-safe
  (old container kept serving) but stalled. The rule — *land a check change in
  its own rev first* — is now **enforced**, not just written down: `path-guard`
  flags a PR whose patch changes a reconcile `check` (regression-tested against
  PR #34, the PR that caused it, which trips it; #33/#39/#40 stay clean).

### Landed with this closeout

- **`OriginCertExpiring`** — the one gap that would have turned a proven
  rollback into an unproven one. Caddy's certificates are never presented to
  production traffic (cloudflared dials `:8085` in plain HTTP; the
  `probe_ssl_earliest_cert_expiry` on the public probes is *Cloudflare's* edge
  cert), so the first real DNS-01 renewal — around **2026-10-08** — could have
  failed in complete silence and surfaced only when break-glass needed a warm
  cert. `caddy-cert-expiry.timer` on llunde-01 now handshakes loopback `:443`
  hourly and stamps `caddy_cert_expiry_timestamp`; the rule fires at 21 days
  remaining, `noDataState: Alerting` because silence is the failure. Runbook
  §13.5. *Not a blackbox probe from llunde-parser: the tailnet ACL scopes
  `tag:server → tag:server` to 9100,9101,3100,4317,4318 and `:443` times out —
  widening that ACL to watch a certificate is the wrong trade.*
- A YAML parse gate on `alerting.yaml` (it is reconcile-watched with no `check`,
  so a mis-indent crash-loops Grafana), and ADR 017's A→CNAME correction carried
  back into the tofu comment that still contradicted it.

### Still open, deliberately

`docs/backlog.md` carries H2/H4–H8, the CI-providers-via-nix item and the
housekeeping list. Two things named here because they are **not** in that file:
the closed-port residual (external callers on the raw IP are invisible from
inside; a day of quiet is the only evidence available and `hcloud firewall
add-rule` is the instant undo — correctly sized, no further spend), and
`README.md`, which still describes the pre-phase-3 repo layout.
