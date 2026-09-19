#!/usr/bin/env bash
set -euo pipefail
dockerfile=${1:?}
target=${2:?}
shift 2
[[ "$dockerfile" != /* && "$dockerfile" != -* && "$dockerfile" != *..* ]]
test -f "$dockerfile"
[[ "$target" =~ ^[a-z][a-z0-9-]*$ ]]
export BUILDKITD_FLAGS="${BUILDKITD_FLAGS:-} --oci-worker-net=host"
CACHE_SCOPE="test-$target" source "$(dirname "${BASH_SOURCE[0]}")/build-cache-args.sh"
buildctl-daemonless.sh build --frontend dockerfile.v0 "${cache_args[@]}" \
  --local "context=${CONTEXT:-.}" --local "dockerfile=$(dirname "$dockerfile")" \
  --opt "filename=$(basename "$dockerfile")" --opt platform=linux/amd64 --opt "target=$target" "$@"
