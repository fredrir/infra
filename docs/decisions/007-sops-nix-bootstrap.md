# ADR 007: sops-nix for Bootstrap Secrets

**Status**: Accepted · **Date**: 2026-08-09

## Context

Runtime secrets live in Doppler ([backend ADR 009]): services get them as plain env vars via `doppler run`. But the host needs credentials *to reach Doppler* — and that bootstrap credential cannot itself come from Doppler. The same chicken-and-egg applies to joining the tailnet (Tailscale auth key) and to opening the backup repository (restic password). A wiped-and-reinstalled host ([ADR 001](001-nixos-declarative-host.md)) must come back able to decrypt these without manual ceremony, or "reproducible" is a lie at the last step.

## Decision

**sops-nix**, with age recipients derived from the host's SSH host key.

- Secrets are committed to this repo **encrypted** (`secrets/*.yaml`, sops/age); each host's age public key (derived from its `ssh_host_ed25519_key.pub` via `ssh-to-age`) is a recipient, alongside the operator's own age key for editing.
- NixOS decrypts at activation into `/run/secrets/<name>` with per-secret owner/mode — e.g. the Doppler token readable only by the service user that wraps its quadlet in `doppler run`.
- **Exactly three secrets live here**: the Doppler service token(s), the Tailscale auth key, and the restic repository password. Everything else stays in Doppler — sops-nix is the bootstrap layer, not a second secret store; scope creep here is a review flag.
- **Install-time wiring**: nixos-anywhere injects the generated SSH host key via `--extra-files` (or the key is captured on first install and the secrets re-encrypted to it), so the very first boot already decrypts. The runbook covers both the fresh-install and the reinstall flow.

## Alternatives considered

- **agenix** — same model, fine choice; sops-nix preferred for its templates, multi-format files, and broader ecosystem. Not a strongly-held preference; recorded so nobody relitigates casually.
- **Manual `scp` of tokens once per host** — un-declarative, undocumented-by-construction, and lost on every reinstall; exactly the failure mode this repo exists to remove.
- **Cloud KMS / Vault** — external infrastructure with its own bootstrap problem, absurd at one-operator scale.

## Consequences

- "Git describes desired state" now includes secret *placement*: which secret exists on which host, with what ownership, is reviewable in the repo; only the ciphertext's content is not.
- Reprovisioning stays two commands + no secret ceremony: the host key travels, decryption follows.
- The operator's age key becomes a real credential to protect — it can edit every bootstrap secret.
- Adding a host = adding one age recipient and re-encrypting (`sops updatekeys`), part of the host-addition runbook step.
- Related: what consumes the Doppler token — the quadlet entrypoint wrapper in [ADR 004](004-quadlet-own-abstraction.md); tailnet join in [ADR 008](008-tailscale-management.md); restic in [ADR 011](011-backups-restic-module.md).
