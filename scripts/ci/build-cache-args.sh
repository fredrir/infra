#!/usr/bin/env bash
set -euo pipefail
cache_args=()
cache_import_args=()
if [[ -n "${BUILDKIT_CACHE_BUCKET:-}" && "${LAYER_CACHE:-true}" == true ]]; then
  : "${BUILDKIT_CACHE_ENDPOINT:?}" "${AWS_ACCESS_KEY_ID:?}" "${AWS_SECRET_ACCESS_KEY:?}" "${IMAGE:?}"
  [[ "$BUILDKIT_CACHE_BUCKET" =~ ^ci-[a-z][a-z0-9-]{0,29}-main$ ]]
  [[ "$BUILDKIT_CACHE_ENDPOINT" =~ ^http://[a-z0-9.-]+:[0-9]+$ ]]
  [[ "$IMAGE" =~ ^ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*$ ]]
  host=${BUILDKIT_CACHE_ENDPOINT#http://}
  if timeout 3 bash -c "exec 3<>/dev/tcp/${host%:*}/${host##*:}" 2>/dev/null; then
    name=${IMAGE#ghcr.io/fredrir/}
    store="type=s3,region=${BUILDKIT_CACHE_REGION:-garage},bucket=$BUILDKIT_CACHE_BUCKET,endpoint_url=$BUILDKIT_CACHE_ENDPOINT,use_path_style=true,prefix=buildkit/,name=${name//\//-}-$(date -u +%G-%V)"
    cache_import_args=(--import-cache "$store")
    cache_args=("${cache_import_args[@]}" --export-cache "$store,mode=max,touch_refresh=1m,ignore-error=true")
    printf 'Layer cache: %s\n' "$BUILDKIT_CACHE_BUCKET" >&2
  else
    printf '::warning::Layer cache unavailable; building without it\n' >&2
  fi
fi
