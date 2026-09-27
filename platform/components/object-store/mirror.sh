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
    source=$(count "$store")
    destination=$(count "$aws")
    [ -n "$source" ] && [ -n "$destination" ] || refuse "object counts unavailable"
    [ $((source * 100)) -ge $((destination * MIN_SOURCE_PERCENT)) ] || refuse "$store holds $source objects, below $MIN_SOURCE_PERCENT% of the $destination in $aws"
    dataset sync "$store" "$aws" --checksum --max-delete=1000 --log-level=NOTICE
    ;;
  seed)
    dataset copy "$aws" "$store" --checksum --transfers=16 --log-level=NOTICE
    dataset check "$aws" "$store" --one-way --size-only
    rclone touch "$store/$sentinel"
    ;;
  restore)
    dataset copy "$aws" "$store" --ignore-existing --transfers=16 --log-level=NOTICE
    ;;
  *)
    refuse "usage: mirror.sh sync|seed|restore"
    ;;
esac
