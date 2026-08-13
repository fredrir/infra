# Backlog — deliberately deferred, not forgotten

Every item was consciously parked, with its reason recorded inline; picking one
up means reading that reason first. Deferred ≠ silently dropped.

## CI resilience

- **Providers via nix** — build the OpenTofu providers through nixpkgs
  (`opentofu.withPlugins` / `terraform-providers.*`) instead of letting `init`
  fetch them from GitHub release assets on every run.

  *Why*: on 2026-08-12 the `tofu` job failed twice on main (503, then `context
  deadline exceeded`) fetching provider `SHA256SUMS` from GitHub release
  downloads, while the same URLs returned 200 from the laptop and GitHub's
  status page read "Actions: Normal" — release-asset delivery is a separate
  component, and throttled runner egress never becomes a declared incident.
  Under pull auto-apply a red gate means no deploys, so a CDN wobble froze the
  estate and `deploy` had to be moved by hand. A `TF_PLUGIN_CACHE_DIR` cache
  landed as the cheap fix, but it only *reduces* registry contact — a cold cache
  still reaches out.

  *Why parked*: nixpkgs' provider versions must agree with
  `tofu/.terraform.lock.hcl`, so adopting it means pinning nixpkgs to matching
  versions or re-locking against whatever nixpkgs ships — a version-management
  commitment, not a one-line change.

## Security / hardening

Residue of the adversarial security review of 2026-08-10.

- **H2** — strip `X-Forwarded-Port` / `X-Forwarded-Ssl` / `Front-End-Https` (and
  friends) at ingress: header variants some frameworks trust that the edge
  currently forwards untouched.
- **H4** — alerting gaps: systemd unit-failed alerts (both hosts), a
  portfolio-backup freshness alert, and a `ResticStale` no-data guard (the
  current alert can't distinguish "no backups" from "no metric"). The
  unit-failed half is a rule, not a project: node_exporter already runs with
  `enabledCollectors = ["systemd"]` on both hosts, so
  `node_systemd_unit_state{state="failed"}` is scraped today and only the
  Grafana rule is missing. Under auto-apply it is arguably the most
  load-bearing alert of the set, since reconcile failures deliberately never
  reboot — they fail-ping and wait for a human.
- **H5** — import the SES sending policy into tofu with a `ses:FromAddress`
  condition (today console-managed; From-pinning is policy, not code).
- **H6** — Prometheus `retention.size` + `MemoryHigh` bounds (60d time retention
  with no size cap on a shared box).
- **H7** — Loki (:3100 push/query + delete API) and OTLP (:4317/4318) are
  unauthenticated listeners. Urgency REDUCED 2026-08-12: after the ACL
  management-scoping, only the estate hosts and the two management devices can
  reach them. The durable fix is still auth (reverse proxy or tenant headers),
  not ACL.
- **H8** — supply-chain: digest-pin/signature-verify the remaining
  `:latest`-riding images (backend/frontend ride `:latest` BY DESIGN for
  pull-based deploys — H8 is about verifying what's pulled, not pinning it).
- **M2, M4–M8, M10 + the low-severity findings** — the codes are all that
  survives. Their detail lived in a session ledger outside this repo which no
  longer exists, so the pointer is dead: re-derive them from a fresh review
  rather than trusting the labels.
- **Replace the laptop's AWS root credentials with a scoped admin user.**
  `~/.aws/credentials`'s `default` profile is the account **root user** —
  `aws sts get-caller-identity` returns `arn:aws:iam::…:root`. Every `aws` call
  and every `tofu` run therefore executes with unrestricted, unscopable,
  un-auditable power. It is the one credential AWS says never to create, and it
  sits directly against the grain of the work around it: `leploy` was fenced off
  tofu state and denied on `restic/*`, and each host got its own scoped restic
  key. Fix: mint a `tofu-admin` IAM user with the permissions the root actually
  needs, switch the profile, then delete the root access key.

  *Why parked*: it was recorded only as an aside in the runbook's prerequisites
  ("reserve it for IAM work and consider replacing later") and never tracked
  anywhere, which is how it survived four phases of otherwise careful
  credential work. Logged here at the 2026-08-13 closeout so it stops being
  invisible.
- **Valkey exporter + `aof_last_bgrewrite_status` alert** — defense in depth
  after the Valkey ownership fix removed the failure mode; the alert would catch
  a regression. Needs a redis_exporter sidecar — batch with other observability
  hardening.

## GitOps / tailnet

- **W3: portfolio → pull-based deploys** (owner decision pending on shape):
  (a) on-host color-flip agent under uid 3000 that polls GHCR and runs the
  existing blue/green logic locally [recommended], (b) drop blue/green for plain
  `AutoUpdate=registry`, (c) status quo. Payoff for a/b: delete the tailnet join
  from portfolio's deploy.yml, the `TS_AUTHKEY` repo secret, ACL rule 3 and
  `tag:ci` — zero CI on the tailnet, zero GitHub-held tailnet credentials.
- **Tailscale reinstall-retag codification** — a fresh nixos-anywhere reinstall
  rejoins untagged (see `tailscale/README.md`): rotate to a reusable
  `tag:server`-capable key + `--advertise-tags`. Routine rebuilds are unaffected.

## Housekeeping

- Prune `s3://llunde-pyparser-bucket/cutover-20260809/` (pre-wipe safety dumps;
  stale once the parser migration was proven — verify before delete).
- Delete the `llunde-parser-old` tailnet node from the console (corpse from the
  llunde-parser reinstall).

## Separate projects (not infra's, tracked so they aren't lost)

- **Frontend API alignment** — llunde-frontend still targets the OLD backend
  API; the alignment project (frontend/backend repos) has never started.
