#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
metrics=/var/lib/node_exporter/textfile_collector
finish() {
  local status=$?
  trap - EXIT
  trap '' TERM INT
  if [[ -n ${work:-} ]] && ! rm -rf "$work"; then
    status=1
  fi
  if ! {
    printf 'platform_control_backup_last_run_success %s\n' "$((status == 0))" > "$metrics/control_backup_status.prom.tmp" &&
    chmod 644 "$metrics/control_backup_status.prom.tmp" &&
    mv "$metrics/control_backup_status.prom.tmp" "$metrics/control_backup_status.prom"
  }; then
    status=1
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
restic cat config > /dev/null
work=$(mktemp -d /var/lib/platform-backups/recovery.XXXXXX)
mkdir "$work/snapshots"
k3s etcd-snapshot save --name platform-recovery --dir "$work/snapshots" --s3=false
test "$(find "$work/snapshots" -maxdepth 1 -type f | wc -l)" -eq 1
install -m600 /var/lib/rancher/k3s/server/token "$work/server-token"
install -m600 /etc/rancher/k3s/agent-token "$work/agent-token"
install -m600 /etc/rancher/k3s/config.yaml "$work/k3s-config.yaml"
k3s kubectl get secrets --all-namespaces -o yaml > "$work/kubernetes-secrets.yaml"
k3s --version > "$work/k3s-version.txt"
(cd "$work" && sha256sum snapshots/* server-token agent-token k3s-config.yaml kubernetes-secrets.yaml k3s-version.txt > SHA256SUMS)
restic --retry-lock 10m backup "$work" --host fredrir-07 --tag control --tag k3s --json
/usr/local/libexec/platform-backup-heartbeat control
printf 'platform_control_backup_last_success_timestamp_seconds %s\n' "$(date +%s)" > "$metrics/control_backup_success.prom.tmp"
chmod 644 "$metrics/control_backup_success.prom.tmp"
mv "$metrics/control_backup_success.prom.tmp" "$metrics/control_backup_success.prom"
