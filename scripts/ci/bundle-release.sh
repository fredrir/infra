#!/usr/bin/env bash
set -euo pipefail
dist=$(cd "$1" && pwd)
summary=$2
notes=$3
bundle=$4
name=$(jq -er .name "$summary")
binary=$(jq -er .binary "$summary")
fail() { printf '::error::%s\n' "$*"; exit 1; }
check=$(mktemp -d)
trap 'rm -rf "$check"' EXIT
(cd "$dist" && sha256sum --check --strict --quiet checksums.txt)
for triple in x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu x86_64-unknown-linux-musl aarch64-unknown-linux-musl x86_64-apple-darwin aarch64-apple-darwin; do
  archives=("$dist/$name-$triple-v"*.tar.gz)
  [[ ${#archives[@]} -eq 1 && -f "${archives[0]}" ]] || fail "expected one $triple archive"
  mkdir "$check/$triple"
  tar -xzf "${archives[0]}" -C "$check/$triple"
  program="$check/$triple/$binary"
  [[ -x "$program" ]] || fail "$triple archive has no executable $binary"
  kind=$(file -b "$program")
  case "$triple" in
    x86_64-*-linux-*) [[ "$kind" == *"ELF 64-bit"*"x86-64"* ]] || fail "$triple is $kind" ;;
    aarch64-*-linux-*) [[ "$kind" == *"ELF 64-bit"*"ARM aarch64"* ]] || fail "$triple is $kind" ;;
    x86_64-apple-darwin) [[ "$kind" == *"Mach-O 64-bit"*"x86_64"* ]] || fail "$triple is $kind" ;;
    aarch64-apple-darwin) [[ "$kind" == *"Mach-O 64-bit"*"arm64"* ]] || fail "$triple is $kind" ;;
  esac
  case "$triple" in
    *-musl) [[ "$kind" == *"statically linked"* || "$kind" == *"static-pie linked"* ]] || fail "$triple is not static" ;;
    *-gnu)
      newest=$(readelf --version-info --wide "$program" | grep -o 'GLIBC_[0-9.]*' | sed 's/GLIBC_//' | sort -V | tail -n1)
      [[ "$(printf '%s\n2.28\n' "$newest" | sort -V | tail -n1)" == 2.28 ]] || fail "$triple needs glibc $newest"
      ;;
  esac
done
generated=(
  "homebrew/Casks/$name.rb:$name.rb"
  "aur/$name-bin.pkgbuild:$name-bin.pkgbuild"
  "aur/$name-bin.srcinfo:$name-bin.srcinfo"
  "aur/$name.pkgbuild:$name.pkgbuild"
  "aur/$name.srcinfo:$name.srcinfo"
  "nix/pkgs/$name/default.nix:$name.nix"
)
rm -rf "$bundle"
mkdir -p "$bundle/assets"
cp "$dist"/*.tar.gz "$dist/checksums.txt" "$bundle/assets/"
for entry in "${generated[@]}"; do
  [[ -s "$dist/${entry%%:*}" ]] || fail "GoReleaser did not generate ${entry%%:*}"
  cp "$dist/${entry%%:*}" "$bundle/assets/${entry##*:}"
done
cp "$summary" "$bundle/assets/release.json"
printf '#!/bin/sh\nset -eu\ncurl --proto "=https" --tlsv1.2 -fsSL https://pkgs.fredrir.com/install.sh | sh -s -- %s "$@"\n' "$name" \
  > "$bundle/assets/$name-installer.sh"
(cd "$bundle/assets" && sha256sum -- "$name.rb" "$name-bin.pkgbuild" "$name-bin.srcinfo" "$name.pkgbuild" "$name.srcinfo" \
  "$name.nix" release.json "$name-installer.sh" >> checksums.txt)
cp "$notes" "$bundle/notes.md"
printf 'Bundled %s release assets\n' "$(find "$bundle/assets" -type f | wc -l)"
