# Phase 4 — task breakdown

House rules as ever: declarative files in parallel; live surfaces (DNS, firewalls, the boxes, tailnet ACLs, GitHub environments) serial by the lead; `nix flake check` green at every step; deviations fix the doc on the spot. Gate metrics throughout: `llunde.no`, `parser.llunde.no`, `hansteen.dev`.

## Workstream G — CI gate (first, before everything)

| # | Task | Proof | Rollback |
|---|---|---|---|
| G1 | `.github/workflows/check.yml` in llunde-infra: on PR + push-to-main, install nix, `nix flake check`, `nix build .#nixosConfigurations.{llunde-01,llunde-parser}.config.system.build.toplevel` (x86_64 runner builds natively; no credentials) | green run on main | delete workflow |
| G2 | Make it a required check on `main`; prove it red-blocks a deliberately broken flake on a test PR | blocked PR screenshot-equivalent | unrequire |

## Workstream O — observability (parallel authoring; lead applies)

| # | Task | Proof | Rollback |
|---|---|---|---|
| O1 | Contract mini-freeze: user `observability` uid 2002 on llunde-parser (20xx block is per-host; reuse across hosts is intended — 2001 already means different services per host), unit list (prometheus, grafana, loki, otel-collector, promtail), tailnet bind addresses/ports, retention (prom 60 d, loki 30 d), `MemoryMax` table (stack ≤2 G), alert delivery (default SES — the zone's DKIM exists), **and the backend-scrape mechanism (review finding: `/metrics` binds 127.0.0.1 on llunde-01 and was never reachable off-box)**: Caddy (already host-network, already reaches the backend loopback) gains a tailnet-bound listener proxying ONLY `/metrics` | contract section committed in this file's header | — |
| O2 | `services/observability/` quadlets + provisioning-as-code (Grafana datasources/dashboards/alert rules from the repo, not clicked) | flake builds; golden for the scrape config | revert |
| O3 | Journal shippers: `services.promtail` (NixOS-native, journal scrape → Loki push over the tailnet, ~100 MB cap) on BOTH hosts — one module, two hosts, no quadlet needed | logs from BOTH hosts answer one Loki query | disable shipper units |
| O4 | Scrapes: both node_exporters + backend `/metrics` via the O1 Caddy tailnet listener; dashboards: host essentials + backend JVM/HTTP + pyparser unit health | live panels for both hosts | — |
| O5 | Alerts wired: `/ready` fail, disk >80 %, restic freshness, portfolio backup freshness, unit-failed; plus the dead-man consideration from ADR 018 (llunde-parser down must still surface) | force one condition (stop a unit) → alert reaches the owner | silence rules |

## Workstream E — edge tunnel (serial where live)

| # | Task | Proof | Rollback |
|---|---|---|---|
| E1 | Create named tunnel `llunde` (dashboard/API); token → new sops secret on llunde-01; ingress config (remote-managed): `llunde.no`→caddy, `www`→caddy, `api`→caddy, all `http://localhost:<caddy-port>`; **plus the E3 test hostname with `httpHostHeader: api.llunde.no`** (review finding: Caddy routes by Host — without the override the test hostname matches no vhost and the proof cannot run) | tunnel healthy, no DNS yet | delete tunnel |
| E2 | `cloudflared` quadlet under `edge`, **`Network=host`** (it dials Caddy on loopback — a podman-networked container's localhost is its own; pyparser's shared-network shape does NOT transfer); Caddy gains a loopback plain-HTTP listener for tunnel traffic which must (review findings, all three load-bearing): **(a) set `X-Forwarded-Proto=https`** (plain-HTTP hop otherwise reports http → Secure cookies, CSRF origin checks and redirects break), **(b) make `CF-Connecting-IP` the rightmost XFF client entry, scoped to THIS listener only** (unscoped, the public-443 path that coexists until E6 lets attackers supply the header; unmapped, every rate-limit bucket keys on 127.0.0.1 — cloudflared's loopback address), **(c) carry the same `@ops` 403 blocks as the public vhosts** (or /metrics goes public through the tunnel); LE paths kept dormant (break-glass needs them) | units green; flake golden for the cloudflared unit incl. the three listener properties | disable unit |
| E3 | **Real-IP + proto verification on the test hostname** (Host-overridden to the real api vhost, per E1): the backend's rotating-XFF proof end-to-end — spoofed XFF AND spoofed `CF-Connecting-IP` from the public edge must NOT mint rate-limit buckets; audit records show true client IPs; **assert Secure-cookie issuance and CSRF acceptance work through the edge** (the X-Forwarded-Proto property) | test transcript in the PR/commit | remove test hostname |
| E4 | Break-glass rehearsal ON PAPER + pre-staged: the two tofu diffs (grey A/AAAA restore; 80/443 reopen) committed as documented patches in the runbook — **including the expected TLS-outage window** (review finding: after grey-flip, Caddy must re-issue LE certs only after DNS propagates AND 80/443 reopen; HSTS makes the interim errors non-bypassable — minutes of hard failure are break-glass WORKING, not broken) | runbook section reviewed | — |
| E5 | 💥 DNS flip in tofu: A/AAAA → proxied tunnel CNAMEs; verify `llunde.no`/`www`/`api` through the edge (full auth lifecycle spot-check against api — cookies + CSRF through CF edge TLS); **re-verify `/metrics`/`/health`/`/ready` 403 through the edge** (E2c); watch one auto-update cycle | all 200/expected; real IPs in audit; ops 403 | pre-staged grey-flip diffs |
| E6 | Close 80/443: tofu firewall + NixOS `publicTCPPorts = []` on llunde-01 → estate-wide zero public inbound; `ss` + timeout probes both boxes | probes time out; sites up | reopen diffs (pre-staged) |

## Workstream A — auto-apply (last; only after G has run ≥1 week or caught a real failure)

| # | Task | Proof | Rollback |
|---|---|---|---|
| A1 | Mint the deploy key (ed25519, single-purpose) + authorize for root on both hosts (declarative: `users.users.root.openssh.authorizedKeys` gains the key — **this first authorization necessarily applies via the manual path**: the workflow cannot bootstrap its own credential); mint `tag:ci` tailnet key; both into GitHub **environment** `infra-apply` | keys nowhere else | remove authorized_keys line + revoke tailnet key |
| A2 | `apply.yml`: needs gate-success, main only, environment `infra-apply`; join tailnet; sequential `nixos-rebuild switch --flake .#llunde-01 …` then `.#llunde-parser`, `--build-host` = target; fail-fast | dry: workflow lints; live: A3 | disable workflow |
| A3 | Prove: trivial merge (comment change) applies to both hosts with zero laptop involvement; then a deliberately failing change on a branch → gate blocks it; then a change that evals but fails at switch (e.g. bad unit) → apply stops, alert fires (workstream O), manual rollback demonstrated | three transcripts | revert commits |
| A4 | Runbook: applies-pause procedure (disable environment), the manual path re-affirmed as recovery, ADR 019 cross-linked | doc review | — |

## Gate

[README](README.md) exit criteria walked top-to-bottom; owner review closes the phase.
