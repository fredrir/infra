#!/usr/bin/env bash
set -euo pipefail
shopt -s nullglob
cd "$1"
keys=$(mktemp -d)
install -m 600 /run/secrets/apk "$keys/fredrir.rsa"
openssl rsa -in "$keys/fredrir.rsa" -pubout -out /etc/apk/keys/fredrir.rsa.pub 2>/dev/null
for arch in x86_64 aarch64; do
  (
    cd "$arch"
    packages=(*.apk)
    [[ ${#packages[@]} -gt 0 ]] || exit 0
    apk index --quiet --rewrite-arch "$arch" --description "fredrir packages" --output APKINDEX.tar.gz -- "${packages[@]}"
    abuild-sign -q -k "$keys/fredrir.rsa" -p fredrir.rsa.pub APKINDEX.tar.gz
  )
done
