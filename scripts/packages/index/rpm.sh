#!/usr/bin/env bash
set -euo pipefail
cd "$1"
GNUPGHOME=$(mktemp -d)
export GNUPGHOME
gpg --batch --quiet --import /run/secrets/gpg
key=$(gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr:/ {print $10; exit}')
for arch in x86_64 aarch64; do
  createrepo_c --quiet --general-compress-type=gz "$arch"
  gpg --batch --yes --local-user "$key" --digest-algo SHA512 --armor --detach-sign \
    --output "$arch/repodata/repomd.xml.asc" "$arch/repodata/repomd.xml"
done
