# ADR 008: Tailscale as the Single Management Overlay

**Status**: Accepted · **Date**: 2026-08-09

## Context

Management access to the llunde hosts needs to survive the NixOS rebuild ([ADR 001](001-nixos-declarative-host.md)) and fit how the owner actually works: routine root SSH sessions, a `ssh llunde` alias, no patience for ceremony. The old estate had two patterns at once — the llunde box exposed port 22 publicly (keys-only), while pyparser was nominally behind a Cloudflare tunnel. The owner floated running both a Cloudflare tunnel *and* Tailscale on the new box.

Two overlays doing the same job is double the moving parts and half the certainty about which path is load-bearing. Notably, the owner can still SSH to pyparser despite its tunnel — almost certainly a Hetzner firewall still allowing 22, i.e. the tunnel never became the only path. That is evidence against "belt and suspenders" setups: the suspenders hid a broken belt for months.

## Decision

**Tailscale is the one management overlay.** The model:

```
laptop → Tailscale (tailnet) → SSH → host
```

- Tailscale runs on every `llunde-*` host, declared in a NixOS module; the auth key is a bootstrap secret from sops-nix ([ADR 007](007-sops-nix-bootstrap.md)).
- Root login **over the tailnet** is accepted — the owner uses it routinely, and the tailnet is the perimeter. The `ssh llunde` alias points at the tailnet name.
- During migration, port 22 stays open to the world, keys-only, as break-glass. Once Tailscale access is proven in daily use, 22 is closed in the Hetzner firewall (a Should in the MoSCoW; the closure is a one-line tofu change).
- **No Cloudflare tunnel on this box.** Cloudflare's tunnel remains pyparser's arrangement until phase 3, whose audit includes checking whether its port 22 is in fact still publicly open ([ADR 014](014-scope-boundaries.md)).

## Alternatives considered

- **Cloudflare tunnel + Tailscale together** — the owner's initial instinct. Rejected: redundant overlays obscure which path matters, and the pyparser evidence shows exactly how a "backup" path lets the primary silently rot.
- **Cloudflare tunnel only** — ties management access to the same vendor as DNS/edge ([ADR 012](012-cloudflare-strategy.md)) and offers a worse SSH experience than a tailnet for a solo operator.
- **Plain port 22 forever** — works, but leaves a permanent public brute-force surface for no benefit once an overlay exists.
- **Bastion host** — a whole extra machine to run what a mesh VPN gives for free at this scale.

## Consequences

- The Hetzner firewall ends at 80/443 (+22 until closure); management traffic never appears on public interfaces after the transition.
- Observability endpoints can bind tailnet-only ([ADR 013](013-observability-host-scope.md)) — the same overlay serves scraping and SSH.
- Tailscale becomes a hard dependency for routine access; the break-glass path after 22 closes is the Hetzner console, documented in the runbook.
- Phase 4's GitOps CI-apply ([ADR 010](010-shared-ci-reusable-workflows.md)) will need a tailnet-connected runner — the overlay choice made here sets that shape.
