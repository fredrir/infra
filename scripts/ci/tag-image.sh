#!/usr/bin/env bash
set -euo pipefail
[[ "$IMAGE" =~ ^ghcr\.io/(fredrir/[a-z0-9][a-z0-9._/-]*)@(sha256:[a-f0-9]{64})$ ]]
name=${BASH_REMATCH[1]}
digest=${BASH_REMATCH[2]}
[[ "$TAG" =~ ^[a-z0-9][a-z0-9.-]{0,127}$ ]]
: "${GITHUB_ACTOR:?}" "${REGISTRY_TOKEN:?}"
registry=${REGISTRY_URL:-https://ghcr.io}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
printf 'user = "%s:%s"\n' "$GITHUB_ACTOR" "$REGISTRY_TOKEN" > "$work/credentials"
unset REGISTRY_TOKEN
token=$(curl --fail --silent --show-error --max-time 30 --config "$work/credentials" \
  "$registry/token?service=ghcr.io&scope=repository:$name:pull,push" | jq -er '.token')
printf 'header = "Authorization: Bearer %s"\n' "$token" > "$work/authorization"
registry_api() { curl --fail --silent --show-error --max-time 60 --config "$work/authorization" "$@"; }

registry_api --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json' \
  --output "$work/manifest" "$registry/v2/$name/manifests/$digest"
printf '%s  %s\n' "${digest#sha256:}" "$work/manifest" | sha256sum --check --strict --quiet
registry_api --request PUT --header "Content-Type: $(jq -er '.mediaType' "$work/manifest")" \
  --data-binary "@$work/manifest" --output /dev/null "$registry/v2/$name/manifests/$TAG"
printf 'Tagged %s as %s\n' "$IMAGE" "$TAG"
