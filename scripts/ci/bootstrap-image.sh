#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
revision=$(git -C "$root" rev-parse HEAD)
engine=$(docker info --format '{{.OSType}}/{{.Architecture}}')
case "$engine" in
linux/x86_64 | linux/amd64) target=amd64 ;;
linux/aarch64 | linux/arm64) target=arm64 ;;
*)
  echo 'A native amd64 or arm64 Linux build engine is required' >&2
  exit 1
  ;;
esac
docker buildx build \
  --platform "linux/$target" \
  --file "$root/.github/images/pipeline/Dockerfile" \
  --tag "infra-ci:$revision-$target" \
  --output "type=oci,dest=$PWD/infra-ci-$revision-$target.tar" \
  "$root/.github/images/pipeline"
