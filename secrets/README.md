# Bootstrap secrets (sops-nix, ADR 007)

Encrypted-at-rest in git; decrypted by the host at activation into `/run/secrets/`.
Contents (see `docs/init/plans/phase-2/contract.md` for the rationale of #4):

1. `doppler.yaml` — Doppler service token(s)
2. `tailscale.yaml` — Tailscale auth key
3. `restic.yaml` — restic repository password
4. `llunde-backend-db.yaml` — Postgres/Valkey internal credentials (env-file form)

Nothing lands here until the phase-2 go-live populates real values with real
recipients (`.sops.yaml` placeholders are replaced first). Never commit a
plaintext secret; `sops` edits only.
