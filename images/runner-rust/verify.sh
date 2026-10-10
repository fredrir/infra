#!/usr/bin/env bash
set -euo pipefail
/home/runner/run.sh --version | grep -x 2.337.0
rustc --version | grep '^rustc 1\.98\.1 '
cargo +1.95.0 --version | grep '^cargo 1\.95\.0 '
zig version | grep -x 0.16.0
cargo-zigbuild -V | grep -F 0.23.4
sccache --version | grep -F 0.18.0
cargo nextest --version | grep -F 0.9.145
cargo audit --version | grep -F 0.22.2
/usr/lib/postgresql/16/bin/initdb --version | grep -F ' 16.'
pkg-config --exists openssl zlib
printf test > /tmp/probe
nix-hash --type sha256 --flat --base32 /tmp/probe | grep -x 020ay2q1av2xs4n842rb3d7vz8qms1dcb87a5yd6azaci20x11lz
test ! -e /etc/ssl/certs/object-store.pem
if grep -qxF "$(sed -n 2p "$OBJECT_STORE_CA_FILE")" /etc/ssl/certs/ca-certificates.crt; then exit 1; fi
grep -qxF "$(sed -n 2p "$OBJECT_STORE_CA_FILE")" /usr/local/share/object-store/trust.pem
grep -qF 'SSL_CERT_FILE=/usr/local/share/object-store/trust.pem exec /usr/local/libexec/sccache' /usr/local/bin/sccache
test "$SCCACHE_IDLE_TIMEOUT" = 0
test -z "${SCCACHE_S3_USE_SSL+set}"
rm -rf /home/runner/_diag /home/runner/run-helper.sh /tmp/probe
