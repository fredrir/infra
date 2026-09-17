#!/usr/bin/env bash
set -euo pipefail
: "${MACOS_SDK_OBJECT:?}" "${MACOS_SDK_SHA256:?}" "${SCCACHE_ENDPOINT:?}" "${AWS_ACCESS_KEY_ID:?}" "${AWS_SECRET_ACCESS_KEY:?}"
[[ "$MACOS_SDK_OBJECT" =~ ^[A-Za-z0-9._-]+$ ]] || { printf '::error::Invalid SDK object name\n'; exit 1; }
work=$(mktemp -d "$RUNNER_TEMP/sdk.XXXXXX")
trap 'rm -rf "$work"' EXIT
umask 077
printf 'user = "%s:%s"\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" > "$work/credentials"
curl --fail --silent --show-error --max-time 600 --config "$work/credentials" \
  --aws-sigv4 "aws:amz:${SCCACHE_REGION:-garage}:s3" \
  --output "$work/sdk.tar.zst" "${SCCACHE_ENDPOINT%/}/toolchains/$MACOS_SDK_OBJECT"
printf '%s  %s\n' "$MACOS_SDK_SHA256" "$work/sdk.tar.zst" | sha256sum --check --strict --quiet
destination="$RUNNER_TEMP/macos-sdk"
rm -rf "$destination"
mkdir -p "$destination"
tar --zstd --extract --file "$work/sdk.tar.zst" --directory "$destination" --no-same-owner
test -f "$destination/MacOSX.sdk/SDKSettings.json"
printf 'macOS SDK %s ready\n' "$(jq -r .Version "$destination/MacOSX.sdk/SDKSettings.json")"
