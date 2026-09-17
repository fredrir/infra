#!/usr/bin/env bash
set -euo pipefail
msrv=$(cargo metadata --no-deps --format-version 1 | jq -r '.packages[].rust_version // empty' | sort -V | tail -n1)
if [[ -z "$msrv" ]]; then
  printf 'No rust-version declared; skipping the MSRV check\n'
  exit 0
fi
[[ "$msrv" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { printf '::error::Unsupported rust-version %s\n' "$msrv"; exit 1; }
toolchain=$(rustup toolchain list | awk '{print $1}' | grep -E "^${msrv//./\\.}(\.[0-9]+)?-" | sort -V | tail -n1 || true)
if [[ -z "$toolchain" ]]; then
  rustup toolchain install "$msrv" --profile minimal --no-self-update
  toolchain=$msrv
fi
cargo "+$toolchain" check --all-targets --locked
