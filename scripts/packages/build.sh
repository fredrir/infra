#!/usr/bin/env bash
set -euo pipefail
: "${PACKAGES_GPG_KEY:?}" "${PACKAGES_APK_KEY:?}" "${RUNNER_TEMP:?}"
here=$(cd "$(dirname "$0")" && pwd)
infra=$(cd "$here/../.." && pwd)
keys=$(mktemp -d "$RUNNER_TEMP/keys.XXXXXX")
trap 'rm -rf "$keys"' EXIT
umask 077
printf '%s\n' "$PACKAGES_GPG_KEY" > "$keys/gpg.key"
printf '%s\n' "$PACKAGES_APK_KEY" > "$keys/fredrir.rsa"
export RPM_SIGNING_KEY="$keys/gpg.key" APK_SIGNING_KEY="$keys/fredrir.rsa"
umask 022
work="$RUNNER_TEMP/packages"
yq --output-format=json "$infra/.github/rust-projects.yaml" > "$RUNNER_TEMP/registry.json"
python3 "$here/collect.py" --registry "$RUNNER_TEMP/registry.json" --work "$work/work" --site "$work/staging" --channels "$work/channels"
for directory in deb/pool rpm/x86_64 rpm/aarch64 apk/x86_64 apk/aarch64; do
  mkdir -p "$work/staging/$directory"
done
mkdir -p "$work/context/index"
mv "$work/staging" "$work/context/site"
cp "$here"/index/*.sh "$work/context/index/"
export BUILDKITD_FLAGS="${BUILDKITD_FLAGS:-} --oci-worker-net=host"
buildctl-daemonless.sh build --frontend dockerfile.v0 \
  --local "context=$work/context" --local "dockerfile=$here" --opt filename=indexes.Containerfile \
  --opt platform=linux/amd64 \
  --secret "id=gpg,src=$keys/gpg.key" --secret "id=apk,src=$keys/fredrir.rsa" \
  --output "type=local,dest=$work/site"
python3 "$here/pages.py" --site "$work/site" --tools "$work/channels/tools.json" \
  --public-gpg "$here/keys/fredrir.asc" --public-apk "$here/keys/fredrir.rsa.pub"
printf 'Site: %s MB\n' "$(du -sm "$work/site" | cut -f1)"
