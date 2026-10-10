#!/usr/bin/env bash
set -euo pipefail
tool=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
"$tool" version
printf "trusted payload\n" > "$work/payload"
COSIGN_PASSWORD=local-qualification "$tool" generate-key-pair --output-key-prefix "$work/key"
COSIGN_PASSWORD=local-qualification "$tool" sign-blob --key "$work/key.key" --bundle "$work/bundle.json" --use-signing-config=false --tlog-upload=false "$work/payload"
"$tool" verify-blob --key "$work/key.pub" --bundle "$work/bundle.json" --insecure-ignore-tlog=true "$work/payload"
printf "tampered payload\n" > "$work/payload"
if "$tool" verify-blob --key "$work/key.pub" --bundle "$work/bundle.json" --insecure-ignore-tlog=true "$work/payload"; then exit 1; fi
