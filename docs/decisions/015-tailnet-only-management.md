# ADR 015: The Tailnet Covers CI Too — Port 22 Retires Everywhere

**Status**: Accepted · **Date**: 2026-08-09 (post-phase-2 planning round)

## Context

[ADR 008](008-tailscale-management.md) made Tailscale the one management overlay and staged port-22 closure as a post-proof Should — written when the only SSH consumer was the owner's laptop. The phase-3 mapping ([research/phase-3-mapping.md](../research/phase-3-mapping.md)) changed the inventory: `llunde-parser` hosts a second production (portfolio) whose CI deploys via forced-command SSH **over public port 22**, and the recreated pyparser CI needs an SSH path of its own. Meanwhile [ADR 010](010-shared-ci-reusable-workflows.md) committed to "no ssh in CI" as the target deploy model — a commitment pyparser cannot meet until its migrations move out of the deploy step ([phase 3.5](../init/plans/phase-3.5/README.md)).

The owner's call: don't defer the closure — include it in phase 3.

## Decision

**Every SSH consumer — human or CI — reaches the hosts over the tailnet, and public port 22 closes on both boxes in phase 3.**

- `llunde-parser` joins the tailnet (imperatively while still Ubuntu; declaratively after 3.5 — same module as `llunde-01`).
- CI jobs that must SSH (pyparser's recreated deploy, portfolio's forced-command deploy) join the tailnet per-run with **ephemeral, `tag:ci`-tagged auth keys**; ACLs limit that tag to port 22 on the two hosts and nothing else. Each consumer holds its own revocable key in its own secret store.
- Closure is ordered, never bundled: tailnet paths proven for laptop **and both CI pipelines** first, then 22 drops from both Hetzner firewalls, `llunde-01`'s NixOS firewall, and `llunde-parser`'s ufw.
- Break-glass after closure is the Hetzner console/rescue system plus a documented one-line tofu re-open — per host, in the runbook.
- **SSH-in-CI is scaffolding, not the model.** ADR 010's pull-based, no-ssh target stands: pyparser's CI-SSH deploy exists only because its migrations run at deploy time; when 3.5 moves alembic to service startup, pyparser converges on auto-update and its CI stops SSHing. portfolio's forced-command deploy is tenant-internal ([ADR 016](016-tenant-slots-host-uniformity.md)) — its transport is our concern, its model is not.

## Alternatives considered

- **Leave 22 open on `llunde-parser` until 3.5** — the initial lean; rejected by the owner. It leaves a public brute-force surface on the two-tenant box precisely during the phase that touches it most.
- **Restrict 22 to GitHub Actions IP ranges** — those ranges are vast and shared; a filter that admits most of a public cloud is not a closure.
- **Tailscale SSH (tailscale-mediated auth)** — more machinery than needed; plain sshd bound behind the tailnet keeps the existing keys, forced commands, and `known_hosts` semantics unchanged.

## Consequences

- The tailscale control plane becomes a deploy-path dependency for both CI pipelines (it already was one for management). Accepted at this scale; the `image_tag`/rollback levers work from the laptop over the same tailnet if CI is ever blind.
- Phase 4's GitOps CI-apply inherits a proven pattern — ephemeral tagged runners were going to be needed anyway ([ADR 008](008-tailscale-management.md) consequences).
- `fail2ban` on `llunde-parser` goes permanently quiet; ufw's remaining job is default-deny.
- The pyparser "is port 22 deliberate?" audit question from [ADR 014](014-scope-boundaries.md)/phase-3's earlier draft is answered by construction: deliberate or not, it closes.
