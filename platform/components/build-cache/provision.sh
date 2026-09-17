#!/usr/bin/env bash
set -euo pipefail

: "${GARAGE_ADMIN_URL:?}" "${GARAGE_ADMIN_TOKEN:?}" "${PROVISIONER_KEY_ID:?}" "${PROVISIONER_KEY_SECRET:?}"
: "${LAYOUT_CAPACITY_BYTES:?}" "${MAIN_QUOTA_BYTES:?}" "${RELEASE_QUOTA_BYTES:?}" "${TOOLCHAINS_QUOTA_BYTES:?}" "${EXPIRATION_DAYS:?}"
KUBERNETES_API_URL=${KUBERNETES_API_URL:-https://kubernetes.default.svc}
SERVICE_ACCOUNT_DIR=${SERVICE_ACCOUNT_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}
WAIT_ATTEMPTS=${WAIT_ATTEMPTS:-60}
WAIT_SECONDS=${WAIT_SECONDS:-5}
PROJECT_LABEL=infra.fredrir.com/build-cache-project
PROVISIONER_KEY_NAME=build-cache-provisioner
NO_PERMISSIONS='{"read":false,"write":false,"owner":false}'

log() { printf '%s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
json() { printf '%s' "$response" | jq "$@"; }

headers=$(mktemp)
trap 'rm -f "$headers"' EXIT
chmod 600 "$headers"
printf 'Authorization: Bearer %s\n' "$GARAGE_ADMIN_TOKEN" > "$headers"

call() {
  local method=$1 path=$2 output
  local args=(--silent --max-time 30 --request "$method" --header "@$headers" --write-out '\n%{http_code}')
  if [[ $# -gt 2 ]]; then
    output=$(printf '%s' "$3" | curl "${args[@]}" --header 'Content-Type: application/json' --data-binary @- "$GARAGE_ADMIN_URL$path") || return 1
  else
    output=$(curl "${args[@]}" "$GARAGE_ADMIN_URL$path") || return 1
  fi
  status=${output##*$'\n'}
  response=${output%$'\n'*}
}

expect() {
  local accepted=$1
  shift
  call "$@" || fail "$1 ${2%%\?*} is unreachable"
  [[ " $accepted " == *" $status "* ]] || fail "$1 ${2%%\?*} returned HTTP $status"
}

require() { expect "200" "$@"; }

wait_for_admin() {
  local attempt
  for ((attempt = 1; attempt <= WAIT_ATTEMPTS; attempt++)); do
    if call GET /v2/GetClusterStatus && [[ $status == 200 ]]; then
      return 0
    fi
    sleep "$WAIT_SECONDS"
  done
  fail "Garage admin API did not become available"
}

ensure_layout() {
  require GET /v2/GetClusterLayout
  json -e '.roles | length > 0' >/dev/null && return 0
  local version node
  version=$(json -er '.version')
  require GET /v2/GetClusterStatus
  node=$(json -er '[.nodes[] | select(.isUp)] | if length == 1 then .[0].id else error("expected exactly one live node") end')
  require POST /v2/UpdateClusterLayout "$(jq -nc --arg id "$node" --argjson capacity "$LAYOUT_CAPACITY_BYTES" \
    '{roles: [{id: $id, zone: "dc1", capacity: $capacity, tags: []}]}')"
  require POST /v2/ApplyClusterLayout "$(jq -nc --argjson version "$((version + 1))" '{version: $version}')"
  log "Applied the single-node cluster layout"
}

project_keys() {
  local token
  token=$(<"$SERVICE_ACCOUNT_DIR/token")
  curl --silent --show-error --fail --max-time 30 --cacert "$SERVICE_ACCOUNT_DIR/ca.crt" \
    --header @- "$KUBERNETES_API_URL/api/v1/namespaces/$(<"$SERVICE_ACCOUNT_DIR/namespace")/secrets?labelSelector=${PROJECT_LABEL//\//%2F}" \
    <<<"Authorization: Bearer $token" \
    | jq -c --arg label "$PROJECT_LABEL" '
      [.items[] | .metadata.labels[$label] as $project | .metadata.name as $name | .data as $data
        | if ($project | test("^[a-z][a-z0-9-]{0,29}$")) and $name == "build-cache-\($project)" then . else error("invalid project secret \($name)") end
        | ("rw", "ro", "release") as $role
        | {project: $project, role: $role, name: "ci-\($project)-\($role)",
           id: ($data["\($role)_id"] // error("\($name) lacks \($role)_id") | @base64d),
           secret: ($data["\($role)_secret"] // error("\($name) lacks \($role)_secret") | @base64d)}]
      | if (map(.id) | unique | length) != length then error("duplicate key ids") else . end'
}

ensure_key() {
  local name=$1 id=$2 secret=$3
  [[ $id =~ ^GK[0-9a-f]{24}$ ]] || fail "key $name has a malformed id"
  [[ $secret =~ ^[0-9a-f]{64}$ ]] || fail "key $name has a malformed secret"
  expect "200 404" GET "/v2/GetKeyInfo?id=$id&showSecretKey=true"
  if [[ $status == 200 ]]; then
    [[ $(json -r '.secretAccessKey') == "$secret" ]] || fail "key $name changed its secret; rotate by generating a new key id"
    if [[ $(json -r '.name') != "$name" ]]; then
      require POST "/v2/UpdateKey?id=$id" "$(jq -nc --arg name "$name" '{name: $name}')"
    fi
    return 0
  fi
  expect "200 409" POST /v2/ImportKey "$(KEY_ID=$id KEY_SECRET=$secret KEY_NAME=$name jq -nc \
    '{accessKeyId: env.KEY_ID, secretAccessKey: env.KEY_SECRET, name: env.KEY_NAME}')"
  [[ $status == 200 ]] || fail "key $name reuses a deleted key id; generate a new key id"
  log "Imported key $name"
}

revoke_stale_keys() {
  local wanted=$1 id name
  require GET /v2/ListKeys
  while IFS=$'\t' read -r id name; do
    [[ -n $id ]] || continue
    require POST "/v2/DeleteKey?id=$id"
    log "Deleted key $name"
  done < <(json -r --argjson wanted "$wanted" '.[] | select((.name // "") | startswith("ci-")) | select(.id as $id | $wanted | any(.[]; . == $id) | not) | [.id, .name] | @tsv')
}

bucket_rules() {
  if [[ $1 == expire ]]; then
    jq -nc --argjson days "$EXPIRATION_DAYS" \
      '[{ID: "expire-build-cache", Status: "Enabled", Expiration: {Days: $days}, AbortIncompleteMultipartUpload: {DaysAfterInitiation: 1}}]'
  else
    printf '[]'
  fi
}

ensure_bucket() {
  local name=$1 quota=$2 rules=$3 grants=$4 id operation key permissions
  expect "200 404" GET "/v2/GetBucketInfo?globalAlias=$name"
  if [[ $status == 404 ]]; then
    require POST /v2/CreateBucket "$(jq -nc --arg name "$name" '{globalAlias: $name}')"
    log "Created bucket $name"
  fi
  id=$(json -er '.id')
  local current
  current=$(json -c '[.keys[] | {key: .accessKeyId, value: .permissions}] | from_entries')
  require POST "/v2/UpdateBucket?id=$id" "$(jq -nc --argjson quota "$quota" --argjson rules "$rules" \
    '{quotas: {maxSize: $quota, maxObjects: null}, lifecycleRules: $rules}')"
  while IFS=$'\t' read -r operation key permissions; do
    [[ -n $operation ]] || continue
    require POST "/v2/${operation}BucketKey" "$(jq -nc --arg bucket "$id" --arg key "$key" --argjson permissions "$permissions" \
      '{bucketId: $bucket, accessKeyId: $key, permissions: $permissions}')"
  done < <(jq -nr --argjson want "$grants" --argjson have "$current" --argjson none "$NO_PERMISSIONS" '
    ($want + $have | keys[]) as $key
    | ($want[$key] // $none) as $w | ($have[$key] // $none) as $h
    | ({read: ($w.read and ($h.read | not)), write: ($w.write and ($h.write | not)), owner: ($w.owner and ($h.owner | not))} | select(any(.[]; .)) | ["Allow", $key, tojson]),
      ({read: ($h.read and ($w.read | not)), write: ($h.write and ($w.write | not)), owner: ($h.owner and ($w.owner | not))} | select(any(.[]; .)) | ["Deny", $key, tojson])
    | @tsv')
}

wait_for_admin
ensure_layout

keys=$(project_keys)
ensure_key "$PROVISIONER_KEY_NAME" "$PROVISIONER_KEY_ID" "$PROVISIONER_KEY_SECRET"
while IFS= read -r entry; do
  ensure_key "$(jq -r .name <<<"$entry")" "$(jq -r .id <<<"$entry")" "$(jq -r .secret <<<"$entry")"
done < <(jq -c '.[]' <<<"$keys")
revoke_stale_keys "$(jq -c '[.[].id]' <<<"$keys")"

grants_for() {
  jq -c --arg project "$1" --arg provisioner "$PROVISIONER_KEY_ID" --arg bucket "$2" '
    {($provisioner): {read: ($bucket == "toolchains"), write: ($bucket == "toolchains"), owner: true}} + ([.[] | select(if $bucket == "toolchains" then .role == "release" else .project == $project end)
      | select(if $bucket == "main" then .role != "release" elif $bucket == "release" then .role == "release" else true end)
      | {key: .id, value: {read: true, write: ($bucket != "toolchains" and .role != "ro"), owner: false}}] | from_entries)' <<<"$keys"
}
while IFS= read -r project; do
  ensure_bucket "ci-$project-main" "$MAIN_QUOTA_BYTES" "$(bucket_rules expire)" "$(grants_for "$project" main)"
  ensure_bucket "ci-$project-release" "$RELEASE_QUOTA_BYTES" "$(bucket_rules expire)" "$(grants_for "$project" release)"
done < <(jq -r '[.[].project] | unique | .[]' <<<"$keys")
ensure_bucket toolchains "$TOOLCHAINS_QUOTA_BYTES" "$(bucket_rules keep)" "$(grants_for "" toolchains)"
log "Build cache is provisioned for $(jq '[.[].project] | unique | length' <<<"$keys") projects"
