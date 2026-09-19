#!/usr/bin/env bash
set -euo pipefail
action=${1:?}
skip() { printf '%s\n' "$1"; exit 0; }
[[ -n "${SCCACHE_ENDPOINT:-}" && -n "${SCCACHE_BUCKET:-}" && -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || skip 'Build output cache is not configured'
[[ "$SCCACHE_BUCKET" =~ ^ci-[a-z][a-z0-9-]{0,29}-(main|release)$ ]]
work=$(mktemp -d "$RUNNER_TEMP/target-cache.XXXXXX")
trap 'rm -rf "$work"' EXIT
umask 077
printf 'user = "%s:%s"\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" > "$work/credentials"
toolchain=$(rustc -vV | sha256sum | cut -c1-16)
object="${SCCACHE_ENDPOINT%/}/$SCCACHE_BUCKET/target/$toolchain"
fingerprint=$(cat Cargo.lock "$RUNNER_TEMP/rust-args.sh" | sha256sum | cut -d' ' -f1)
limit=${RUST_TARGET_CACHE_LIMIT_KIB:-8388608}

s3() {
  curl --fail --silent --show-error --connect-timeout 5 --max-time 600 --config "$work/credentials" \
    --aws-sigv4 "aws:amz:${SCCACHE_REGION:-garage}:s3" "$@"
}

case "$action" in
  restore)
    s3 --output "$work/target.tar.zst" "$object.tar.zst" 2>/dev/null || skip 'No cached build outputs'
    tar --zstd --extract --file "$work/target.tar.zst" --no-same-owner || { rm -rf target; skip '::warning::Cached build outputs are unreadable'; }
    printf 'Restored %s of build outputs\n' "$(du -sh target | cut -f1)"
    ;;
  save)
    [[ "${SCCACHE_S3_RW_MODE:-READ_WRITE}" == READ_WRITE ]] || skip 'Build output cache is read-only'
    [[ -d target ]] || skip 'No build outputs'
    [[ "$(s3 "$object.fingerprint" 2>/dev/null || true)" != "$fingerprint" ]] || skip 'Cached build outputs are current'
    if (( $(du -sk target | cut -f1) > limit )); then
      s3 --request DELETE "$object.tar.zst" || true
      s3 --request DELETE "$object.fingerprint" || true
      skip 'Build outputs outgrew the cache; the next run repopulates it'
    fi
    tar --use-compress-program 'zstd -T0 -3' --create --file "$work/target.tar.zst" target
    printf '%s' "$fingerprint" > "$work/fingerprint"
    s3 --upload-file "$work/target.tar.zst" "$object.tar.zst" && s3 --upload-file "$work/fingerprint" "$object.fingerprint" \
      || skip '::warning::Build outputs were not cached'
    printf 'Cached %s of build outputs\n' "$(du -sh "$work/target.tar.zst" | cut -f1)"
    ;;
  *) exit 1 ;;
esac
