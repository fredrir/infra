#!/usr/bin/env bash
# Set up the public share origin external.llunde.no (see CLOUDFLARE.md):
#   1. proxied CNAME external.llunde.no -> <tunnel>.cfargotunnel.com
#   2. tunnel ingress: external.llunde.no ^/media/share/.* -> http://review:8081,
#      plus a hostname-scoped 404 fallback — inserted BEFORE the global catch-all.
# Idempotent: each step is skipped if already present. Touches ONLY the remote
# tunnel *configuration* — never the tunnel or its token (rotating that drops
# the site; see the warning in CLOUDFLARE.md).
#
# Requires jq + CLOUDFLARE_API_TOKEN with:
#   Zone / DNS / Edit             (zone llunde.no)
#   Account / Cloudflare Tunnel / Edit
set -euo pipefail

ACCOUNT=8786559b30fcebd08d0c594b6e899eef
ZONE=4ae54b24fc4140d4d1c450491645f1c8
TUNNEL=e77d6ebf-dcfb-4ade-b4eb-2be0d9e165a9
HOST=external.llunde.no
API=https://api.cloudflare.com/client/v4
auth=(-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H "Content-Type: application/json")

existing=$(curl -sf "$API/zones/$ZONE/dns_records?name=$HOST" "${auth[@]}" | jq '.result | length')
if [ "$existing" = "0" ]; then
  curl -sf -X POST "$API/zones/$ZONE/dns_records" "${auth[@]}" --data "{
    \"type\": \"CNAME\", \"name\": \"$HOST\",
    \"content\": \"$TUNNEL.cfargotunnel.com\", \"proxied\": true,
    \"comment\": \"pyparser Convert public share origin\"
  }" > /dev/null
  echo "dns: created $HOST"
else
  echo "dns: $HOST exists, skipping"
fi

config=$(curl -sf "$API/accounts/$ACCOUNT/cfd_tunnel/$TUNNEL/configurations" "${auth[@]}" | jq '.result.config')
if [ "$config" = "null" ]; then
  echo "ingress: no remote config found — tunnel is not remotely managed as expected; use the dashboard" >&2
  exit 1
fi
if echo "$config" | jq -e --arg h "$HOST" '.ingress[] | select(.hostname == $h)' > /dev/null; then
  echo "ingress: $HOST rules exist, skipping"
else
  merged=$(echo "$config" | jq --arg h "$HOST" '
    .ingress = .ingress[:-1] + [
      {hostname: $h, path: "^/media/share/.*", service: "http://review:8081"},
      {hostname: $h, service: "http_status:404"}
    ] + .ingress[-1:]')
  curl -sf -X PUT "$API/accounts/$ACCOUNT/cfd_tunnel/$TUNNEL/configurations" \
    "${auth[@]}" --data "{\"config\": $merged}" > /dev/null
  echo "ingress: added $HOST rules"
fi

echo "verify: curl -sI 'https://$HOST/media/share/<token>/<img>.png'  -> 200"
echo "        curl -s -o /dev/null -w '%{http_code}\n' https://$HOST/  -> 404"
