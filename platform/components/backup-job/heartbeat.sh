#!/usr/bin/env bash
set -Eeuo pipefail
[[ -n ${BACKUP_HEARTBEAT_TOKEN:-} ]] || exit 0
case ${1:-} in
  parser|y|portfolio|attic|control) ;;
  *) exit 2 ;;
esac
[[ "$BACKUP_HEARTBEAT_TOKEN" =~ ^[A-Za-z0-9_-]{32,}$ ]]
umask 077
config=$(mktemp)
trap 'rm -f "$config"' EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
printf 'header = "Authorization: Bearer %s"\n' "$BACKUP_HEARTBEAT_TOKEN" > "$config"
curl --disable --fail --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 20 --config "$config" --request POST --output /dev/null "http://100.86.241.75:8080/api/v1/endpoints/backups_$1/external?success=true"
