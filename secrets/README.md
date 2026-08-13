# Bootstrap secrets (sops-nix, ADR 007)

Encrypted at rest in git; decrypted by the host at activation into
`/run/secrets/` with per-secret owner/group. These are the credentials a host
needs before it can reach anything else — everything an app reads at runtime
lives in Doppler instead ([docs/doppler.md](../docs/doppler.md)).

Wiring (which secret lands where, with what owner) is in
`hosts/<host>/secrets.nix`; the mechanism is `modules/secrets/`.

| File | Keys | Host | What |
|---|---|---|---|
| `doppler.yaml` | `doppler_token` | llunde-01 | Doppler `llunde/prd` service token, env-file form → `/run/secrets/doppler.env` |
| `pyparser-doppler.yaml` | `doppler_token` | llunde-parser | Doppler `pyparser/prd` token, read by the root render oneshot — never by a container |
| `tailscale.yaml` | `auth_key` | **both** | Tailscale auth key |
| `ghcr.yaml` | `auth_json` | **both** | containers-auth.json for private GHCR pulls (mode 0640, group `ghcr`) |
| `restic.yaml` | `password`, `env` | llunde-01 | restic repo password + env-file form AWS creds for S3 |
| `pyparser-restic.yaml` | `password`, `env` | llunde-parser | same, for the `restic/llunde-parser` prefix |
| `llunde-backend-db.yaml` | `env` | llunde-01 | `POSTGRES_PASSWORD`/`DB_PASSWORD`, shared by the postgres unit and the backend |
| `llunde-tunnel.yaml` | `env` | llunde-01 | `TUNNEL_TOKEN` for the llunde tunnel, owner `edge` |
| `llunde-caddy-acme.yaml` | `env` | llunde-01 | `CF_API_TOKEN` for ACME DNS-01, owner `edge` |
| `llunde-caddy-ghcr.yaml` | `auth_json` | llunde-01 | `edge`-only GHCR credential for the repo-built `ghcr.io/fredrir/llunde-caddy` |
| `observability-smtp.yaml` | `env` | llunde-parser | Grafana SES SMTP credential (send-only, From-pinned to alerts@llunde.no) |
| `observability-grafana.yaml` | `env` | llunde-parser | Grafana admin password — kills the `admin:admin` default if a login method is ever re-enabled |
| `gitops-llunde-01.yaml` | `deploy_key`, `heartbeat_url` | llunde-01 | read-only repo deploy key + this host's healthchecks.io ping URL |
| `gitops-llunde-parser.yaml` | `deploy_key`, `heartbeat_url` | llunde-parser | same for llunde-parser — the heartbeat matters most here, since the observability stack itself lives on this box |

## Recipients

Three age keys, declared in `.sops.yaml`:

- **admin** — the owner's personal age key (`~/.config/sops/age/keys.txt`,
  generated 2026-08-09). On every file, so the owner can always edit.
- **llunde-01** — derived from that host's `ssh-ed25519` host key.
- **llunde-parser** — derived from a **pre-generated** host key, injected at
  install via `nixos-anywhere --extra-files` so secrets decrypt on first boot.

The `creation_rules` are ordered, and the order is load-bearing: every specific
rule — `gitops-llunde-parser`, `(pyparser|observability)-*`, and the shared
`(tailscale|ghcr)` pair — must stay **before** the `secrets/.*\.yaml` catch-all,
which encrypts to admin + llunde-01 only. Slip one behind it and those secrets
never decrypt on the box that needs them. A host can decrypt only its own files;
`tailscale.yaml` and `ghcr.yaml` are the two both hosts share.

## Rotation

- Edit values with `sops secrets/<file>.yaml` — **never** commit a plaintext
  secret, and never edit these files with a plain editor.
- After changing recipients in `.sops.yaml`, re-encrypt each affected file with
  `sops updatekeys secrets/<file>.yaml`.
- Reinstalling a host changes its SSH host key, and with it its age recipient:
  either pre-generate and inject the key as above, or update `.sops.yaml` and
  `updatekeys` every file that host reads.
- ⚠️ `llunde-tunnel.yaml` holds a Cloudflare **tunnel token**, whose value
  embeds the tunnel secret. Replacing the file is fine; *regenerating the token
  at Cloudflare* drops the live connectors and takes the site down
  ([docs/cloudflare.md](../docs/cloudflare.md)).
