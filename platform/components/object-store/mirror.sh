#!/bin/sh
set -eu
store=store:parser-dataset
aws=aws:llunde-pyparser-bucket
sentinel=.mirror-seeded
refuse() {
  echo "parser dataset mirror refused: $1" >&2
  exit 1
}
dataset() {
  operation=$1
  shift
  rclone "$operation" "$@" '--include=/files/**' '--include=/extract/**' '--include=/assets/**' '--include=/convert/**' --fast-list
}
count() {
  dataset size "$1" --json | sed -n 's/.*"count": *\([0-9][0-9]*\).*/\1/p'
}
case "${1:-}" in
  sync)
    : "${MIN_SOURCE_PERCENT:?}"
    rclone lsf --files-only --max-depth 1 "--include=/$sentinel" "$store" | grep -qx "$sentinel" || refuse "$store has no $sentinel; seed or restore it first"
    if [ -n "${SEED_NOT_BEFORE:-}" ]; then
      printf '%s\n' "$SEED_NOT_BEFORE" | grep -Eqx '[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}' || refuse "SEED_NOT_BEFORE is not a UTC time like 2026-09-27 12:00:00"
      seeded=$(rclone lsf --files-only --max-depth 1 --format t "--include=/$sentinel" "$store")
      [ "$(printf '%s\n%s\n' "$SEED_NOT_BEFORE" "$seeded" | sort | head -n 1)" = "$SEED_NOT_BEFORE" ] || refuse "$store was seeded at ${seeded:-an unknown time}, before the parser left it at $SEED_NOT_BEFORE; seed it again"
    fi
    source=$(count "$store")
    destination=$(count "$aws")
    [ -n "$source" ] && [ -n "$destination" ] || refuse "object counts unavailable"
    [ $((source * 100)) -ge $((destination * MIN_SOURCE_PERCENT)) ] || refuse "$store holds $source objects, below $MIN_SOURCE_PERCENT% of the $destination in $aws"
    dataset sync "$store" "$aws" --checksum --max-delete=1000 --buffer-size=4M --log-level=NOTICE
    ;;
  seed)
    dataset copy "$aws" "$store" --checksum --transfers=8 --buffer-size=4M --log-level=NOTICE
    dataset check "$aws" "$store" --one-way --size-only
    rclone delete --max-depth 1 "--include=/$sentinel" "$store"
    rclone touch "$store/$sentinel"
    ;;
  restore)
    dataset copy "$aws" "$store" --ignore-existing --transfers=8 --buffer-size=4M --log-level=NOTICE
    ;;
  unseed)
    rclone delete --max-depth 1 "--include=/$sentinel" "$store"
    remaining=$(rclone lsf --files-only --max-depth 1 "--include=/$sentinel" "$store") || refuse "cannot list $store to confirm $sentinel is gone"
    [ -z "$remaining" ] || refuse "$store still has $sentinel"
    ;;
  *)
    refuse "usage: mirror.sh sync|seed|restore|unseed"
    ;;
esac
