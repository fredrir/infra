# ADR 020: GitOps — Pull Auto-Apply (supersedes ADR 019)

**Status**: Accepted · **Date**: 2026-08-11 · Supersedes [ADR 019](019-gitops-auto-apply.md) (push auto-apply, accepted but never built)

## Context

ADR 019 committed to auto-apply via push: a GitHub Actions workflow holding a
root SSH deploy key and a tailnet key, applying to both hosts over the tailnet.
The owner rejected that credential posture before implementation: no standing,
estate-reaching credential lives in GitHub. A second independent review
endorsed a pull model with conditions, and — importantly — corrected the
framing this ADR now records honestly.

## Framing corrections (what pull does and does not buy)

1. **Pull does not eliminate the GitHub risk.** Under merge-is-deploy, the
   GitHub **account** is root-equivalent on the estate in *any* model — a
   compromised account merges a malicious commit and the hosts apply it
   faithfully. Pull removes *standing, exfiltratable* credentials from GitHub,
   removes third-party Actions from the trust path, and removes the
   survives-account-recovery asymmetry (a leaked SSH key keeps working after
   the account is recovered; a malicious commit stops mattering once reverted).
   The residual is an account-security problem, not an architecture problem.
2. **Secret-elimination is not why pull beats push.** Tailscale Workload
   Identity Federation + Tailscale SSH could make push near-secret-free too.
   Pull wins because **a host applying config to itself can self-heal a
   severed activation** — it holds a local rollback lever (the previous
   generation plus a deadman reboot) at the exact moment the network is gone.
   Push structurally cannot: ADR 019's own guardrail 4 admitted a severed
   apply "can neither confirm nor roll back".
3. **Staying fully manual was an honest second place.** The gate (check.yml)
   already closed the only real correctness gap ("nothing validated main").
   The owner chose pull, twice, with the machinery cost on the table.

## Decision

**Each host deploys itself; nothing deploys to it.** Tofu stays manual from
the laptop (no GitHub-OIDC path for existing Hetzner/Cloudflare projects; low
churn; solo plan-review is better locally).

1. **The gate is the only promoter.** `promote.yml` fast-forwards the
   unprotected `deploy` branch to a main commit only after `Check` passes on
   it — FF-only, via the ephemeral `GITHUB_TOKEN`. Hosts track `deploy`, never
   `main`: they only ever apply gate-green commits.
2. **Hosts pull with a read-only, per-host deploy key** over
   `git+ssh://…?ref=deploy&rev=<sha>` — rev-pinned (immutable, so no
   tarball-ttl staleness; FF-only promotion keeps every pinned rev reachable),
   git+ssh because the `github:` flakeref scheme is the HTTPS tarball API and
   cannot authenticate with a deploy key. GitHub's host key is pinned
   declaratively. Compromise of a key yields repo *read* access only, and each
   host's key revokes independently.
3. **Self-healing apply** (`modules/gitops-pull`, per host, every 5 min):
   build fail-closed → record `attempting` → arm a transient **reboot
   deadman** (10 min) → `switch-to-configuration test` (bootloader untouched —
   the boot default IS the rollback) → **reachability probes decide** (tailnet
   up or peer path alive, sshd listening, repo fetchable; retried, two
   consecutive passes) → `switch` + verify the boot default moved → reconcile
   → heartbeat. Probes are management-plane only, deliberately: the deadman's
   job is "never lose the box", app health is the alerting stack's job, and
   every extra probe is a new way to reboot prod on a flake.
4. **Failure memory.** A rev that failed to confirm is *held* — after the
   deadman's rollback reboot the host refuses to re-apply it (no reboot loop)
   until a new rev lands on `deploy` or the hold is cleared by hand.
5. **Post-switch reconcile.** NixOS's switch reloads user managers but never
   restarts changed user services, and podman resolves bind mounts at
   container creation — so the module hashes a declared watch-set (quadlet
   unit files, bind-mounted configs) across the switch and restarts exactly
   what changed. Under manual deploys a human did this (runbook §6); under
   auto-apply, forgetting it would make merges silently not deploy the most
   common class of change. Restart failures alert; they never reboot (the new
   generation is already the boot default — app-plane, alerting territory).
6. **Canary lag, no coordination.** llunde-01 applies immediately;
   llunde-parser refuses revs younger than 30 min — longer than the deadman
   window, so the canary has self-healed (and alerted) or confirmed before the
   second host touches the same rev. Cross-host blessing protocols were
   rejected: real coupling for marginal gain at two hosts.
7. **Per-host external heartbeat** (healthchecks.io, URL in sops): success
   ping every cycle; failures curl `<url>/fail` with a journal tail. This is
   deliberately external and per-host: the observability stack lives ON
   llunde-parser — nothing else watches the watcher.

## Operations (runbook §6/§9 are the authority)

- **Pause**: disable the *Promote to deploy* workflow (freezes both hosts), or
  `systemctl stop gitops-pull.timer` on one host. Manual deploys: stop the
  timer first.
- **Rollback**: `git push -f <good-sha>:deploy` from the laptop — works with
  the tailnet down; `deploy` is deliberately unprotected so this lever always
  exists. The next FF promote from main proceeds normally afterwards.
- **Hold clear** (after a deadman rollback): push a fixed rev, or
  `rm /var/lib/gitops-pull/attempting` on the host.

## Accepted residuals — named, not hand-waved

- **Boot-plane failures.** A config that activates but cannot *boot*
  (kernel/initrd/bootloader) is outside the loop: `test` + probes exercise
  activation, never a boot, and nixos-25.11 has no systemd-boot boot counting.
  Bounded by the gate building both closures and by boot-plane changes riding
  deliberate flake.lock bumps. Recovery: Hetzner web console → boot menu →
  previous generation. Corollary: after a lock bump auto-applies, the next
  reboot boots an untested kernel — same recovery.
- **`deploy` is unprotected.** An account compromise can move it past the
  gate. Subsumed by the account-root residual above (the same account merges
  to main); the mitigation is account security (hardware MFA, minimal PATs),
  which no architecture choice replaces.
- **Probe false negatives.** Tailscale control down *and* the peer path dead
  during the probe window rolls back a good config: one spurious reboot, a
  hold, an email. Accepted over probe complexity.
- **App-plane breakage rolls forward.** A config that keeps the management
  plane alive but breaks a service is Prometheus/Grafana's to catch.

## Alternatives considered

- **Push with OIDC-federated ephemeral credentials** — viable now (Tailscale
  WIF), but cannot self-heal a severed activation; rejected on the self-heal
  property, not on secrets.
- **Stay manual + gate** — free, honest second place; declined by the owner.
- **repository_dispatch / dispatch-triggered pull** — an inbound trigger
  surface with no gain over polling; rejected.
- **Tofu auto-apply** — rejected: static cloud secrets in GitHub (no OIDC for
  existing Hetzner/CF projects), low churn, and plan-review belongs on the
  laptop. Not even plan-on-PR (it would surface tfstate into CI logs).
