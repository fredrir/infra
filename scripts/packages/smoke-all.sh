#!/usr/bin/env bash
set -euo pipefail
site=$(cd "$1" && pwd)
channels=$(cd "$2" && pwd)
scope=$3
here=$(cd "$(dirname "$0")" && pwd)
context=$(mktemp -d)
trap 'rm -rf "$context"' EXIT
cp -R "$site" "$context/site"
cp "$here/smoke.sh" "$context/"
quick=(
  "public.ecr.aws/docker/library/debian:12@sha256:6ebd97fa83deb272194a2cf015b3d26a4d538e9ad3a7a79d544c8af5b0a01443 deb"
  "quay.io/fedora/fedora:44@sha256:e7d398c0a67572b33a43260102f9135ab79b6b8d95efca91faa5179d83b2cdf0 rpm"
  "public.ecr.aws/docker/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b apk"
)
full=(
  "${quick[@]}"
  "public.ecr.aws/docker/library/ubuntu:22.04@sha256:829f6df217bcbae2b371026e81711d1a787c61b2967ad09d015063663ebafbf7 deb"
  "public.ecr.aws/docker/library/ubuntu:24.04@sha256:69cecf4bbf72d2d44a9eef1b71fb98c7fb973d78af11399deccef19beb008ad9 deb"
  "quay.io/rockylinux/rockylinux:9@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f rpm"
  "registry.opensuse.org/opensuse/leap:16.0@sha256:6a8998a33df6164d29d545c1bb8d9dd5a3595206d993b1a43c54de9aa33d8feb rpm"
)
[[ "$scope" == full ]] && targets=("${full[@]}") || targets=("${quick[@]}")
mapfile -t tools < <(jq -r 'to_entries[] | "\(.key) \(.value.binary)"' "$channels/tools.json")
[[ ${#tools[@]} -gt 0 ]] || { echo "No packages to test"; exit 0; }
export BUILDKITD_FLAGS="${BUILDKITD_FLAGS:-} --oci-worker-net=host"
for target in "${targets[@]}"; do
  read -r base format <<< "$target"
  for tool in "${tools[@]}"; do
    read -r package binary <<< "$tool"
    printf '%s on %s: ' "$package" "${base%%@*}"
    buildctl-daemonless.sh build --frontend dockerfile.v0 --progress plain \
      --local "context=$context" --local "dockerfile=$here" --opt filename=smoke.Containerfile \
      --opt platform=linux/amd64 --opt "build-arg:BASE=$base" --opt "build-arg:FORMAT=$format" \
      --opt "build-arg:PACKAGE=$package" --opt "build-arg:BINARY=$binary" > "$context/log" 2>&1 \
      || { echo failed; tail -40 "$context/log"; exit 1; }
    echo ok
  done
done
