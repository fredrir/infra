# ADR 019: GitOps — Full Auto-Apply on Merge

**Status**: Superseded by [ADR 020](020-gitops-pull-auto-apply.md) (2026-08-11 — the owner rejected this push design's credential posture before implementation; nothing here was ever built) · **Date**: 2026-08-09 (phase-4 re-evaluation) · Executes the revisit [ADR 010](010-shared-ci-reusable-workflows.md) deferred

## Context

ADR 010 kept host applies manual ("CI never holds production access of any kind until phase 4 revisits it deliberately"). This is that deliberate revisit. The lead's re-evaluation recommended stopping at a credential-free CI gate (flake check + closure builds) and keeping applies manual; **the owner chose full auto-apply on merge**. The pattern's building blocks are all proven in-estate: `tag:ci` ephemeral tailnet joins (ADR 015), `--target-host/--build-host` rebuilds (exercised ~15 times in one day during phase 3.5), NixOS generations as rollback, and the environment-secrets discipline (the phase-3 shadowing lesson).

## Decision

**Merge to llunde-infra `main` applies the configuration to both hosts automatically**, with these load-bearing guardrails:

1. **Gate before apply, always**: the apply job runs only after the credential-free gate job (`nix flake check` + both closures build) passes. The gate is also a required PR check — broken flakes never reach main in the first place.
2. **Credentials live in a GitHub *environment*** (`infra-apply`): a dedicated ed25519 deploy key (authorized for root on both hosts) plus an ephemeral `tag:ci` tailscale key. Environment scoping is deliberate: repo-level secrets shadow nothing, and the environment can gain protection rules later without workflow changes.
3. **Sequential, fail-fast**: hosts apply one at a time (llunde-01 then llunde-parser); the first failure stops the sequence. `--build-host` keeps builds on the target — the runner never builds or uploads closures.
4. **The manual path is the recovery route and must always work**: `nixos-rebuild switch --flake .#<host> --target-host` from the laptop, unchanged, documented in the runbook. CI is a convenience on top, never a gatekeeper (the backend's justfile principle, again).
   **Network-plane changes go through the manual path by policy** (plan-review finding): a change touching tailscaled, sshd, or the firewall can sever the CI's own session mid-`switch`, leaving activation state the workflow can neither confirm nor roll back — its "failure" on such a change means *verify manually*, never retry.
5. **Failures notify from the workflow itself** (second-opinion correction of the original "alert through observability" direction — a failed apply may have broken exactly that stack, on a host the sequence never reached): a failure step in `apply.yml` notifies directly; observability alerting is the belt, not the suspenders. Guardrail 4's network-plane policy gets **teeth as a CI path-guard** (changes under tailscale/profile/firewall paths fail the apply job with a manual-path pointer), all actions are SHA-pinned (the credentialed workflow's most plausible compromise path is a hijacked third-party action), and applies serialize under a concurrency group.
6. **Blast-radius honesty**: this puts root-equivalent access to the estate in GitHub's trust domain (Actions + the environment's secrets). Accepted by the owner with eyes open; mitigations are the tailnet ACL (`tag:ci` reaches only the two hosts' SSH), the environment boundary, and single-purpose keys that revoke in two places (authorized_keys line + tailnet key) without touching anything else.

## Alternatives considered

- **Eval+build gate only, manual applies** — the lead's recommendation: closes the real gap (nothing validated main) with zero credentials. Declined by the owner in favor of full automation; the gate survives as guardrail 1.
- **Dispatch-triggered apply** — same credentials, fire-on-click. Declined: if the credential exists at all, the owner prefers the full loop.
- **colmena / deploy-rs** — multi-host tooling for a two-host estate; plain `nixos-rebuild` per host is fewer moving parts. Still rejected (ADR 010's phase-4 note anticipated revisiting at larger host counts).

## Consequences

- Infra changes deploy like app changes: merge-is-deploy, estate-wide — the "Git describes desired state" principle finally closes its last loop.
- A compromised GitHub account or Actions supply chain can now touch the hosts; the environment + ACL + revocable-key design bounds and audits that exposure but does not eliminate it.
- Rollback story: revert the commit (auto-applies the previous config) or `nixos-rebuild --rollback` / generation pick over the tailnet — all pre-existing.
- Applies during an incident: pause by disabling the workflow or the environment — documented in the runbook alongside break-glass.
