#!/usr/bin/env bash
set -euo pipefail
catalog=images/catalog.yaml
registry=${REGISTRY_URL:-https://ghcr.io}
shared=$(git rev-parse --verify HEAD:.github/workflows/images.yml)$(git rev-parse --verify HEAD:.dockerignore)

published() {
  local token status
  token=$(curl --fail --silent --max-time 30 "$registry/token?service=ghcr.io&scope=repository:$1:pull" | jq -er '.token') || return 1
  status=$(curl --silent --head --max-time 30 --output /dev/null --write-out '%{http_code}' \
    --header "Authorization: Bearer $token" \
    --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json' \
    "$registry/v2/$1/manifests/$2") || return 1
  [[ "$status" == 200 ]]
}

pending=''
count=$(yq -e 'length' "$catalog")
for ((index = 0; index < count; index++)); do
  entry=$(yq -e -o=json -I=0 ".[$index]" "$catalog")
  image=$(jq -er '.image' <<< "$entry")
  [[ "$image" =~ ^ghcr\.io/(fredrir/[a-z0-9][a-z0-9._/-]*)$ ]]
  name=${BASH_REMATCH[1]}
  objects=''
  while IFS= read -r path; do
    object=$(git rev-parse --verify "HEAD:$path")
    objects+="$object"$'\n'
  done < <(jq -er '.inputs[]' <<< "$entry")
  [[ -n "$objects" ]]
  tag=inputs-$(printf '%s\n%s\n%s' "$entry" "$shared" "$objects" | sha256sum | cut -d' ' -f1)
  if published "$name" "$tag"; then
    printf 'Unchanged: %s\n' "$image"
  else
    printf 'Building: %s\n' "$image"
    pending+=$(jq -c --arg tag "$tag" 'del(.inputs) + {tag: $tag}' <<< "$entry")$'\n'
  fi
done
printf 'images=%s\n' "$(jq -sc '.' <<< "$pending")" >> "$GITHUB_OUTPUT"
