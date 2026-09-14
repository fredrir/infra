#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
trap 'echo "backup.sh: line $LINENO failed: $BASH_COMMAND" >&2' ERR

work=${BACKUP_WORK_DIR:-/work}
service_account=${BACKUP_SERVICE_ACCOUNT_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}
files=${BACKUP_FILES_DIR:-/files}
preflight_timeout=${BACKUP_PREFLIGHT_TIMEOUT:-120}
export_timeout=${BACKUP_EXPORT_TIMEOUT:-600}
upload_timeout=${BACKUP_UPLOAD_TIMEOUT:-900}
quiesce_timeout=${BACKUP_QUIESCE_TIMEOUT:-180}
kill_grace=${BACKUP_KILL_GRACE:-120}

mkdir -p "$work/source"
read -r -a writers <<< "${BACKUP_WRITERS:-}"
kube=(kubectl --request-timeout=10s)
if (( ${#writers[@]} )); then
  namespace=$(cat "$service_account/namespace")
  [[ "$namespace" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]
  test -r "$service_account/ca.crt"
  test -r "$service_account/token"
  export KUBECONFIG="$work/kubeconfig"
  jq -n --arg directory "$service_account" --arg namespace "$namespace" '{
    apiVersion: "v1", kind: "Config",
    clusters: [{name: "cluster", cluster: {server: "https://kubernetes.default.svc", "certificate-authority": ($directory + "/ca.crt")}}],
    users: [{name: "backup", user: {tokenFile: ($directory + "/token")}}],
    contexts: [{name: "backup", context: {cluster: "cluster", user: "backup", namespace: $namespace}}],
    "current-context": "backup"
  }' > "$KUBECONFIG"
fi

resume() {
  local failed=0 name replicas current
  [[ -f "$work/resume.tsv" ]] || return 0
  while read -r name replicas; do
    current=$("${kube[@]}" get deployment "$name" -o jsonpath='{.spec.replicas}') || { failed=1; continue; }
    if [[ "$current" == 0 ]]; then
      "${kube[@]}" scale deployment "$name" --current-replicas=0 --replicas="$replicas" || failed=1
    elif [[ "$current" != "$replicas" ]]; then
      failed=1
    fi
  done < "$work/resume.tsv"
  return "$failed"
}

cleanup() {
  local status=$?
  trap - EXIT
  trap '' TERM INT
  if ! resume; then
    echo 'Writer resume failed; manual recovery required' >&2
    status=1
  fi
  exit "$status"
}

quiet() {
  local name selector count
  for name in "${writers[@]}"; do
    [[ $("${kube[@]}" get deployment "$name" -o jsonpath='{.spec.replicas}') == 0 ]]
    selector=$("${kube[@]}" get deployment "$name" -o json | jq -er '.spec.selector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")')
    count=$("${kube[@]}" get pods -l "$selector" -o json | jq -er '.items | length')
    [[ "$count" == 0 ]]
  done
}

sqlite_snapshot() {
  local source=$1 target=$2 attempt
  for attempt in 1 2 3; do
    rm -f "$target"
    if sqlite3 "$source" "VACUUM INTO '$target'"; then
      return 0
    fi
    echo "backup.sh: sqlite snapshot attempt $attempt failed; retrying" >&2
    sleep 10
  done
  return 1
}

if [[ ${1:-} == export ]]; then
  trap cleanup EXIT
  trap 'exit 143' TERM
  trap 'exit 130' INT
  : > "$work/resume.tsv"
  for name in "${writers[@]}"; do
    [[ "$name" =~ ^[a-z0-9-]+$ ]]
    replicas=$("${kube[@]}" get deployment "$name" -o jsonpath='{.spec.replicas}')
    [[ "$replicas" == 0 || "$replicas" == 1 ]]
    if [[ "$replicas" == 1 ]]; then
      printf '%s %s\n' "$name" "$replicas" >> "$work/resume.tsv"
      "${kube[@]}" scale deployment "$name" --current-replicas=1 --replicas=0
    fi
  done
  for name in "${writers[@]}"; do
    selector=$("${kube[@]}" get deployment "$name" -o json | jq -er '.spec.selector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")')
    "${kube[@]}" wait --for=delete pods -l "$selector" --timeout="${quiesce_timeout}s"
  done
  quiet
  case "$BACKUP_KIND" in
    postgres)
      pg_dump --format=custom --no-owner --no-acl --file="$work/source/database.dump"
      pg_restore --list "$work/source/database.dump" > /dev/null
      ;;
    mongodb)
      jq -n '{uri:env.MONGODB_URI}' > "$work/mongodb.json"
      mongodump --config="$work/mongodb.json" --archive="$work/source/database.archive.gz" --gzip 2> "$work/mongodump.log"
      grep -q 'done dumping' "$work/mongodump.log"
      mongorestore --config="$work/mongodb.json" --archive="$work/source/database.archive.gz" --gzip --dryRun > /dev/null
      rm "$work/mongodb.json" "$work/mongodump.log"
      ;;
    sqlite)
      sqlite_snapshot "$files/metadata.db" "$work/source/metadata.db"
      [[ $(sqlite3 "$work/source/metadata.db" 'PRAGMA integrity_check;') == ok ]]
      ;;
    *) exit 2 ;;
  esac
  if [[ ${BACKUP_LOCAL_FILES:-false} == true ]]; then
    local_bytes=$(du -sb "$files" | cut -f1)
    if (( local_bytes > ${BACKUP_LOCAL_FILES_MAX_BYTES:?} )); then
      echo "backup.sh: $files is $local_bytes bytes, over the $BACKUP_LOCAL_FILES_MAX_BYTES scratch budget; raise the work volume before it evicts mid-tar" >&2
      exit 1
    fi
    exclude=()
    [[ -z ${BACKUP_LOCAL_FILES_EXCLUDE:-} ]] || exclude=(--exclude="$BACKUP_LOCAL_FILES_EXCLUDE")
    tar --create --file="$work/source/local-files.tar" --one-file-system "${exclude[@]}" --directory="$files" .
  fi
  quiet
  (cd "$work/source" && sha256sum ./* > SHA256SUMS)
  exit 0
fi

timeout --signal=TERM --kill-after=30 "$preflight_timeout" restic cat config > /dev/null
trap cleanup EXIT
timeout --signal=TERM --kill-after="$kill_grace" "$export_timeout" bash "$0" export &
child=$!
trap 'kill -TERM "$child" 2>/dev/null || true; wait "$child" || true; exit 143' TERM INT
wait "$child"
trap - TERM INT
timeout --signal=TERM --kill-after="$kill_grace" "$upload_timeout" restic --retry-lock 10m backup "$work/source" --host platform --tag "$BACKUP_PROJECT" --tag "$BACKUP_KIND" --json
bash "$(dirname "$0")/heartbeat.sh" "$BACKUP_PROJECT" || echo 'backup.sh: heartbeat failed' >&2
