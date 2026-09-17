#!/usr/bin/env bash
set -euo pipefail
label=infra.fredrir.com/ci-slots
resource=infra.fredrir.com/ci-slot
reconcile() {
  kubectl get nodes --selector "$label" --output json \
    | jq -r --arg label "$label" --arg resource "$resource" \
      '.items[] | [.metadata.name, .metadata.labels[$label], (.status.capacity[$resource] // "")] | @tsv' \
    | while IFS=$'\t' read -r node wanted advertised; do
        if [[ ! "$wanted" =~ ^[0-9]+$ ]]; then
          printf 'Ignoring %s: slot label %s is not a count\n' "$node" "$wanted"
        elif [[ "$advertised" != "$wanted" ]]; then
          kubectl patch node "$node" --subresource=status --type=merge \
            --patch "{\"status\":{\"capacity\":{\"$resource\":\"$wanted\"}}}" >/dev/null
          printf 'Advertised %s CI slots on %s (was %s)\n' "$wanted" "$node" "${advertised:-none}"
        fi
      done
}
while true; do
  reconcile
  [[ -z "${RECONCILE_ONCE:-}" ]] || exit 0
  sleep "${RECONCILE_SECONDS:-30}"
done
