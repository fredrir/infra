# Bootstrap secrets (sops-nix, ADR 007)

Encrypted-at-rest in git; decrypted by the host at activation into `/run/secrets/`.
Contents (see `docs/init/plans/phase-2/contract.md` for the rationale of #4–5):

1. `doppler.yaml` — key `doppler_token`, env-file form (`DOPPLER_TOKEN=...`)
2. `tailscale.yaml` — key `auth_key`
3. `restic.yaml` — keys `password` and `env` (env-file form: AWS creds for S3)
4. `llunde-backend-db.yaml` — key `env`, env-file form (`POSTGRES_PASSWORD`/`DB_PASSWORD`)
5. `ghcr.yaml` — key `auth_json`, a containers-auth.json for private GHCR pulls (group `ghcr`)

Nothing lands here until the phase-2 go-live populates real values with real
recipients (`.sops.yaml` placeholders are replaced first). Never commit a
plaintext secret; `sops` edits only.
