# Cloudflare — pyparser (tunnel + Access)

The `parser.llunde.no` / `external.llunde.no` **DNS records are in tofu**
(`tofu/modules/cloudflare/`, [../cloudflare.md](../cloudflare.md)). The tunnel
resource, its ingress map and the Access applications are **not**, deliberately:
the connector authenticates with `TUNNEL_TOKEN`, which embeds the tunnel secret,
so letting tofu manage the tunnel would rotate that secret on `apply` and drop
the live connectors. Manage those through the Cloudflare dashboard / API.

- **Account:** `8786559b30fcebd08d0c594b6e899eef`
- **Zone (`llunde.no`):** `4ae54b24fc4140d4d1c450491645f1c8`
- **Team domain:** `hidden-pond-1118.cloudflareaccess.com` → `PYPARSER_CF_ACCESS_TEAM_DOMAIN`

## Tunnel

- Name `pyparser-review`, id `e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9`
- Ingress (order matters — most specific path first):
  - `parser.llunde.no` → `http://review:8081`; fallback `http_status:404`
  - a legacy `^/logs(/.*)?$` → `http://dozzle:8080` rule predates the NixOS
    reinstall; Dozzle and its docker-socket proxy are **retired**, so that route
    has no backend (logs are journald over the tailnet — [PROD.md](PROD.md))
- Connector: the `pyparser-cloudflared` quadlet (`services/pyparser/`), reading
  `TUNNEL_TOKEN` from `/run/pyparser/secrets.env`, rendered from Doppler
  `pyparser/prd`
- DNS: proxied CNAME `parser.llunde.no` → `<tunnel-id>.cfargotunnel.com` (record
  id `0d50fbb4e9e2f35cbca26553d142e868`), declared in tofu

## Access applications

- **pyparser review** — `parser.llunde.no`, app id `04579775-b789-4058-b742-2b944c1a235a`
  - AUD `ba66847d37cafcff8b7400c78988d406f11c3adfa3f72cced66247bf85531d59` → `PYPARSER_CF_ACCESS_AUD`
  - Policy `admins` (id `b9e78de1-6bc4-463a-9c21-103565d96922`): allow the email allowlist (`PYPARSER_ADMIN_EMAILS`)
- **pyparser /parser** — `parser.llunde.no/parser`, app id `cf1f78d5-0573-46d6-ad55-0a6c6828bc9f`
  - AUD `ff3d82a81b1957ffb1d72c2189ea804d85997c48e2d686c8a063fdc5844ec4c0`
  - Policy `backend-service-token` (id `626c22a1-98eb-42c7-af28-fed956399eb9`, non-identity): the `pyparser-backend` service token (id `34355a0b-74d1-4ea2-a3b4-6f3ac5ce4dc6`) — how the Node backend reaches `/parser/*`

## Public share origin — `external.llunde.no`

The Convert page's explicitly-shared images (capability-token URLs) get a
**dedicated public hostname** instead of an Access bypass on `parser.llunde.no`,
so the review host keeps the invariant "everything here requires Access" with no
carve-out apps to misconfigure.

- DNS: proxied CNAME → `<tunnel-id>.cfargotunnel.com` (same tunnel)
- Tunnel ingress, before the fallback: `external.llunde.no` path
  `^/media/share/.*` → `http://review:8081`; `external.llunde.no` (no path) →
  `http_status:404`
- **Deliberately NO Access application.** Safety is app-side: requests here hold
  no Access JWT, so the review app's auth middleware 401s everything except
  `/media/share/`, and its share-origin guard 404s every non-share path on this
  host outright (`src/pyparser/review/auth/middleware.py`).
- The app mints shared markdown URLs on this origin via
  `PYPARSER_SHARE_BASE_URL` (Doppler `pyparser/prd`). Adding a hostname to the
  tunnel does **not** touch the tunnel secret.

## ⚠️ Do not rotate the tunnel secret

The live connectors authenticate with the token in `/run/pyparser/secrets.env`.
Regenerating the tunnel or its token means updating Doppler `pyparser/prd`,
re-rendering and restarting the connector, or the site goes down. If the tunnel
is ever brought into tofu, pin the *existing* secret and set
`lifecycle { ignore_changes = [secret] }` on it.
