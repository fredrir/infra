# Cloudflare (llunde.no) — managed outside Terraform

Like pyparser, llunde.no sits behind a **Cloudflare Tunnel** (cloudflared dials out;
no public web ports on the origin). Managed via the Cloudflare dashboard/API, **not**
Terraform — the `TUNNEL_TOKEN` (in `/opt/llunde/.env`) embeds the tunnel secret, so
letting Terraform manage it would rotate the secret and drop the connectors. The
Terraform in this dir manages only the server + an SSH-only firewall.

- Account: `8786559b30fcebd08d0c594b6e899eef` · Zone (`llunde.no`): `4ae54b24fc4140d4d1c450491645f1c8`

## Tunnel
- Name `llunde`, id `ed8abcdb-508c-4d61-85b7-bc3127510e4b`
- Ingress: `llunde.no` + `www.llunde.no` → `http://nginx:80` (internal docker network); else `http_status:404`
- Connector: `cloudflared tunnel run` in `backend/infra/docker-compose.prod.yml`; `TUNNEL_TOKEN` from `/opt/llunde/.env`
- DNS: proxied CNAME `llunde.no` + `www.llunde.no` → `ed8abcdb-…cfargotunnel.com`

## Origin
- nginx is **HTTP-only on :80** (serves the SPA from `/srv/frontend` + proxies `/api` → `backend:8000`). Cloudflare terminates TLS at the edge — **no origin certs / certbot**.
- The Terraform host firewall allows **inbound SSH only**; the origin is unreachable on 80/443 from the internet.

## ⚠️ Do not rotate the tunnel secret
The connectors authenticate with the `TUNNEL_TOKEN` in `/opt/llunde/.env`. Regenerating the tunnel/token requires updating `.env` + redeploying, or the site drops.

## Known follow-ups (pre-existing, not part of the tunnel work)
- **Feide login unconfigured**: `FEIDE_CLIENT_ID` / `FEIDE_CLIENT_SECRET` / `FEIDE_REDIRECT_URI` are blank in `/opt/llunde/.env` — waiting on keys.
- **`deploy-llunde.yml` is stale**: it rsyncs a non-existent `deploy/` dir (real assets live in `backend/infra/`), references a non-existent `deploy.yml` in its `paths:`, and does not deploy `cloudflared`/`TUNNEL_TOKEN`. Fix before relying on CI deploys; the frontend was last deployed manually (build + rsync to `/opt/llunde/frontend-dist`).
