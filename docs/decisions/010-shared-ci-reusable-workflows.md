# ADR 010: Shared CI as Reusable Workflows; Pull-Based Deploys

**Status**: Accepted · **Date**: 2026-08-09 · Host applies resolved by [ADR 020](020-gitops-pull-auto-apply.md)

> The deferred question — should CI apply host configuration — was answered in a
> way that keeps this ADR's central property intact: hosts poll a gate-green
> `deploy` ref and apply themselves, so CI still holds no production access.

## Context

Each service repo needs near-identical CI: build an image from a Containerfile, push it to GHCR. Duplicating that per repo drifts, and the owner explicitly wants llunde-infra to own the deployment-related CI. The old monorepo workflows (see [../research/current-state.md](../research/current-state.md)) did builds *and* deploys: buildx pushes to GHCR, then an SSH job that rsynced assets and ran `docker compose up` on the host — CI holding SSH keys to production.

Meanwhile the runtime side already decided pull-based updates: Quadlet units reference `:latest` and per-user `podman-auto-update` timers poll GHCR ([ADR 004](004-quadlet-own-abstraction.md)).

## Decision

**llunde-infra hosts reusable GitHub workflows (`workflow_call`) in `.github/workflows/`; service repos keep ~10-line callers.**

- `build-image.yml` — the one shared workflow now: checkout, build from the repo's Containerfile, push `:sha` and `:latest` to GHCR, with optional build args sourced from Doppler (the frontend needs Vite-time values such as the old Turnstile site key; they live in a Doppler CI config, not GitHub secrets).
- **Deployment is pull-based and CI-free**: the auto-update timers on the host are the deploy mechanism. No SSH keys, no rsync, no host commands in any workflow — the old deploy job's entire second half is deleted, not ported.
- The old separate **migrate image is obsolete**: the new backend runs Flyway at application startup (backend ADR 003), so one image per service suffices.
- `deploy-now.yml` — an optional tailnet poke for impatient moments — is a **Could**, not built now.
- CI-applied NixOS config (true GitOps for the *host*) is deliberately deferred to phase 4 ([ADR 014](014-scope-boundaries.md)); until then `nixos-rebuild --target-host` from the laptop applies host changes.

## Alternatives considered

- **Composite actions** instead of reusable workflows — finer-grained but each repo still owns job wiring; reusable workflows centralize the whole job. Rejected.
- **CI-push deploys (SSH from Actions)** — the old pattern; gives CI production credentials and duplicates what auto-update already does. Rejected.
- **Keep CI fully per-repo** — three drifting copies today, more with every future service. Rejected.
- **GitOps host-apply now** — wants a tailnet-connected runner and host credentials in CI; deferred until the estate is stable (phase 4).

## Consequences

- Adding a service = a Containerfile + a caller workflow + a Nix service module; CI logic changes happen in exactly one repo.
- Callers reference `fredrir/llunde-infra/.github/workflows/build-image.yml@main` — infra CI changes propagate to all services on merge.
- Deploy latency is the auto-update timer interval; if that ever annoys, `deploy-now.yml` is the escape hatch.
- CI never holds production access of any kind until phase 4 revisits it deliberately.
