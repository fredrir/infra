#!/bin/sh
set -eu
printf 'visible-cpus=%s\n' "$(nproc)"
test "$(id -u)" = 0
export HOME=/tmp/build-home XDG_CONFIG_HOME=/tmp/build-config CARGO_HOME=/build/cargo-home CARGO_TARGET_DIR=/build/target
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
export CARGO_BUILD_JOBS=2 CARGO_INCREMENTAL=0
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$CARGO_HOME"
apk add --no-cache libcap-ng-static=0.8.5-r2 libseccomp-static=2.6.0-r2 musl-dev=1.2.6-r2 gcc=15.2.0-r5 binutils=2.45.1-r1 pkgconf=2.5.1-r0
cp /lib/apk/db/installed /output/apk-installed
rustc -vV > /output/rustc.txt
cargo -vV > /output/cargo.txt
test "$(rustc --version | cut -d ' ' -f2)" = 1.98.1
export RUSTFLAGS='-C target-feature=+crt-static -C link-self-contained=yes'
export LIBSECCOMP_LINK_TYPE=static LIBSECCOMP_LIB_PATH=/usr/lib LIBCAPNG_LINK_TYPE=static LIBCAPNG_LIB_PATH=/usr/lib
cd /source
cargo rustc --locked --release --target x86_64-unknown-linux-musl --bin virtiofsd -- -C link-arg=-Wl,-Map,/output/link.map
cp /build/target/x86_64-unknown-linux-musl/release/virtiofsd /output/virtiofsd
cp Cargo.lock /output/Cargo.lock
cargo metadata --locked --format-version 1 > /output/cargo-metadata.json
cargo tree --locked --target x86_64-unknown-linux-musl -e features > /output/cargo-features.txt
readelf -lW /output/virtiofsd > /output/elf-program-headers.txt
readelf -d /output/virtiofsd > /output/elf-dynamic.txt
if grep -q INTERP /output/elf-program-headers.txt; then exit 1; fi
if grep -q NEEDED /output/elf-dynamic.txt; then exit 1; fi
sha256sum /usr/lib/libseccomp.a /usr/lib/libcap-ng.a > /output/native-library-hashes.txt
find "$(rustc --print sysroot)/lib/rustlib/x86_64-unknown-linux-musl/lib/self-contained" -type f -exec sha256sum '{}' ';' >> /output/native-library-hashes.txt
sha256sum /output/virtiofsd /output/Cargo.lock /output/apk-installed /output/link.map > /output/checksums.txt
