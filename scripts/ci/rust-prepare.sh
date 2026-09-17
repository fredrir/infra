#!/usr/bin/env bash
set -euo pipefail
arguments() {
  local name=$1 value=$2
  [[ "$value" =~ ^[A-Za-z0-9_,=:/.\ -]*$ ]] || { printf '::error::%s contains unsupported characters\n' "$name"; exit 1; }
  read -ra flags <<< "$value"
  for flag in "${flags[@]}"; do
    [[ "$flag" != --manifest-path* && "$flag" != --config* && "$flag" != -Z* ]] || { printf '::error::%s may not set %s\n' "$name" "$flag"; exit 1; }
  done
  declare -p flags | sed "s/declare -a flags=/declare -a ${name}=/"
}
clippy=$(arguments CLIPPY_FLAGS "${CLIPPY_ARGS:-}") || { printf '%s\n' "$clippy"; exit 1; }
test=$(arguments TEST_FLAGS "${TEST_ARGS:-}") || { printf '%s\n' "$test"; exit 1; }
printf '%s\n%s\n' "$clippy" "$test" > "$RUNNER_TEMP/rust-args.sh"
if [[ -n "${RUSTC_WRAPPER:-}" ]]; then
  host=${SCCACHE_ENDPOINT#*://}
  host=${host%%/*}
  if timeout 3 bash -c "exec 3<>/dev/tcp/${host%:*}/${host##*:}" 2>/dev/null && sccache --start-server >/dev/null 2>&1; then
    printf 'Compilation cache: %s (%s)\n' "$SCCACHE_BUCKET" "${SCCACHE_S3_RW_MODE:-READ_WRITE}"
  else
    printf '::warning::Build cache unavailable; compiling without it\n'
    printf 'RUSTC_WRAPPER=\n' >> "$GITHUB_ENV"
  fi
fi
rustup show active-toolchain
