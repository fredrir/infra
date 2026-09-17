#!/usr/bin/env bash
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { printf 'Run this on an Apple machine\n' >&2; exit 1; }
: "${SOPS_AGE_KEY_FILE:?}" "${KUBECONFIG:?}"
root=$(cd "$(dirname "$0")/../.." && pwd)
sdk=$(cd "$(xcrun --sdk macosx --show-sdk-path)" && pwd -P)
version=$(xcrun --sdk macosx --show-sdk-version)
object="MacOSX${version}.sdk.tar.zst"
work=$(mktemp -d)
cleanup() {
  [[ -n "${forward:-}" ]] && kill "$forward" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
umask 077
tar -C "$(dirname "$sdk")" -s "|^$(basename "$sdk")|MacOSX.sdk|" -cf - "$(basename "$sdk")" | zstd -19 -T0 -q -o "$work/$object"
digest=$(shasum -a 256 "$work/$object" | cut -d' ' -f1)
secret="$root/platform/components/build-cache/provisioner.secret.sops.yaml"
printf 'user = "%s:%s"\n' \
  "$(sops decrypt --extract '["stringData"]["id"]' "$secret")" \
  "$(sops decrypt --extract '["stringData"]["secret"]' "$secret")" > "$work/credentials"
kubectl --namespace build-cache port-forward service/garage 13900:3900 >/dev/null &
forward=$!
for _ in $(seq 30); do nc -z 127.0.0.1 13900 2>/dev/null && break; sleep 1; done
curl --fail --silent --show-error --config "$work/credentials" --aws-sigv4 "aws:amz:garage:s3" \
  --upload-file "$work/$object" "http://127.0.0.1:13900/toolchains/$object"
printf 'MACOS_SDK_OBJECT: %s\nMACOS_SDK_SHA256: %s\n' "$object" "$digest"
