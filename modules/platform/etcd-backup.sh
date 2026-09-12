#!/usr/bin/env bash
set -euo pipefail
umask 077
test "$(id -u)" = 0
data_dir=${PLATFORM_K3S_DATA_DIR:-/var/lib/rancher/k3s}
metrics_dir=${PLATFORM_METRICS_DIR:-/var/lib/platform/metrics}
: "${RESTIC_REPOSITORY_FILE:?Repository file required}"
: "${RESTIC_PASSWORD_FILE:?Password file required}"
test -s "$RESTIC_REPOSITORY_FILE"
test -s "$RESTIC_PASSWORD_FILE"
test -s "$data_dir/server/token"
snapshot_name="recovery-$(date -u +%Y%m%dT%H%M%S)-$$"
k3s etcd-snapshot save --data-dir "$data_dir" --name "$snapshot_name"
snapshot_path=$(find "$data_dir/server/db/snapshots" -maxdepth 1 -type f -name "$snapshot_name*" -print -quit)
test -n "$snapshot_path"
bundle=$(mktemp -d "$data_dir/etcd-backup.XXXXXX")
trap 'rm -rf "$bundle"; rm -f "$snapshot_path"' EXIT
install -m600 "$snapshot_path" "$bundle/snapshot"
install -m600 "$data_dir/server/token" "$bundle/server-token"
version=$(k3s --version | head -n1 | cut -d' ' -f3)
[[ "$version" =~ ^v1\.[0-9]+\.[0-9]+\+k3s[0-9]+$ ]]
snapshot_hash=$(sha256sum "$bundle/snapshot" | cut -d' ' -f1)
token_hash=$(sha256sum "$bundle/server-token" | cut -d' ' -f1)
printf '{"schemaVersion":1,"kind":"k3s-etcd","k3sVersion":"%s","createdAt":%s,"files":{"snapshot":"%s","server-token":"%s"}}\n' "$version" "$(date +%s)" "$snapshot_hash" "$token_hash" > "$bundle/manifest.json"
tar -C "$bundle" -cf - . | restic backup --stdin --stdin-filename platform-etcd.tar --tag platform-etcd
install -d -m755 "$metrics_dir"
metric_file=$(mktemp "$metrics_dir/k3s-etcd.XXXXXX")
printf 'restic_last_success_timestamp{backup_job="k3s-etcd"} %s\n' "$(date +%s)" > "$metric_file"
chmod 644 "$metric_file"
mv "$metric_file" "$metrics_dir/k3s-etcd-success.prom"
