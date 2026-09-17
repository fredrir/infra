#!/usr/bin/env bash
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { printf 'Run this on an Apple machine\n' >&2; exit 1; }
usage() { printf 'usage: %s package DIRECTORY | upload ARCHIVE\n' "$0" >&2; exit 1; }
root=$(cd "$(dirname "$0")/../.." && pwd)
case "${1:-}" in
  package)
    output=${2:?$(usage)}
    sdk=$(cd "$(xcrun --sdk macosx --show-sdk-path)" && pwd -P)
    archive="$output/MacOSX$(xcrun --sdk macosx --show-sdk-version).sdk.tar.zst"
    mkdir -p "$output"
    tar -C "$(dirname "$sdk")" -s "|^$(basename "$sdk")|MacOSX.sdk|" -cf - "$(basename "$sdk")" | zstd -19 -T0 -q -o "$archive"
    printf 'MACOS_SDK_OBJECT: %s\nMACOS_SDK_SHA256: %s\n' "$(basename "$archive")" "$(shasum -a 256 "$archive" | cut -d' ' -f1)"
    ;;
  upload)
    archive=${2:?$(usage)}
    : "${SOPS_AGE_KEY_FILE:?}" "${KUBECONFIG:?}"
    work=$(mktemp -d)
    trap 'kill "${forward:-}" 2>/dev/null || true; rm -rf "$work"' EXIT
    umask 077
    secret="$root/platform/components/build-cache/provisioner.secret.sops.yaml"
    printf 'user = "%s:%s"\n' \
      "$(sops decrypt --extract '["stringData"]["id"]' "$secret")" \
      "$(sops decrypt --extract '["stringData"]["secret"]' "$secret")" > "$work/credentials"
    kubectl --namespace build-cache port-forward service/garage 13900:3900 >/dev/null &
    forward=$!
    for _ in $(seq 30); do nc -z 127.0.0.1 13900 2>/dev/null && break; sleep 1; done
    curl --fail --silent --show-error --config "$work/credentials" --aws-sigv4 "aws:amz:garage:s3" \
      --upload-file "$archive" "http://127.0.0.1:13900/toolchains/$(basename "$archive")"
    printf 'Uploaded %s\n' "$(basename "$archive")"
    ;;
  *) usage ;;
esac
