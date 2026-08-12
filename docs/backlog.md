# Backlog — deliberately deferred, not forgotten

Every item here was consciously parked with a reason; picking one up means
reading its provenance first (the phase-4 adversarial review of 2026-08-10
for the H/M items — findings summarized in the PR #10–#13 era discussions).
Deferred ≠ silently dropped: this file is the estate's memory of what it
still owes itself.

**In flight (not backlog):** the E5/E6 edge cutover + its prep (B2/B3/H1/M3)
— handover written 2026-08-12, owner-gated. M3 (cutover-visible CF-Ray probe)
lands WITH E5, not from this list.

## Security / hardening (phase-4 review residue)

- **H2** — strip `X-Forwarded-Port` / `X-Forwarded-Ssl` / `Front-End-Https`
  (and friends) at ingress: header variants some frameworks trust that the
  edge currently forwards untouched.
- **H4** — alerting gaps: systemd unit-failed alerts (both hosts), a
  portfolio-backup freshness alert, and a `ResticStale` no-data guard (the
  current alert can't distinguish "no backups" from "no metric").
- **H5** — import the SES sending policy into tofu with a `ses:FromAddress`
  condition (today it's console-managed; From-pinning is policy, not code).
- **H6** — Prometheus `retention.size` + `MemoryHigh` bounds (60d time
  retention with no size cap on a shared box).
- **H7** — Loki (:3100 push/query + delete API) and OTLP (:4317/4318) are
  unauthenticated listeners. Urgency REDUCED 2026-08-12: after the ACL
  management-scoping, only the estate hosts and the two management devices
  can reach them. The durable fix is still auth (reverse proxy or tenant
  headers), not ACL.
- **H8** — supply-chain: digest-pin/signature-verify the remaining
  `:latest`-riding images (backend/frontend ride `:latest` BY DESIGN for
  pull-based deploys — H8 is about verifying what's pulled, not pinning it).
- **M2, M4–M8, M10 + low-severity findings** — details preserved in the
  2026-08-10 entry of the session ledger
  (`~/.claude/projects/-Users-fredrir-llunde-new-backend/memory/llunde-project-state.md`);
  expand into this file when picked up.
- **Valkey exporter + `aof_last_bgrewrite_status` alert** — C2's
  defense-in-depth follow-on (the ownership fix removed the failure mode;
  the alert would catch a regression). Needs a redis_exporter sidecar —
  batch with other observability hardening.

## GitOps / tailnet

- **W3: portfolio → pull-based deploys** (owner decision pending on shape):
  (a) on-host color-flip agent under uid 3000 that polls GHCR and runs the
  existing blue/green logic locally [recommended], (b) drop blue/green for
  plain `AutoUpdate=registry`, (c) status quo. Payoff for a/b: delete the
  tailnet join from portfolio's deploy.yml, the `TS_AUTHKEY` repo secret,
  ACL rule 3 and `tag:ci` — zero CI on the tailnet, zero GitHub-held tailnet
  credentials.
- **Tailscale reinstall-retag codification** — a fresh nixos-anywhere
  reinstall rejoins untagged (see `tailscale/README.md` §Codification):
  rotate to a reusable `tag:server`-capable key + `--advertise-tags`.
  Routine rebuilds are unaffected.

## Housekeeping

- Prune `s3://llunde-pyparser-bucket/cutover-20260809/` (pre-wipe safety
  dumps; stale once the parser migration was proven — verify before delete).
- Delete the `llunde-parser-old` tailnet node from the console (corpse from
  the phase-3.5 rename).

## Separate projects (not infra's, tracked so they aren't lost)

- **Frontend API alignment** — llunde-frontend still targets the OLD backend
  API; the alignment project (frontend/backend repos) has never started.
