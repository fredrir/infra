#!/usr/bin/env bash
set -euo pipefail
[[ $# == 4 && $(id -u) != 0 && $(uname -s) == Linux ]]
recovery_name=$(basename -- "$4")
[[ $recovery_name =~ ^infra-online-restore\.([a-f0-9]{12})$ ]]
recovery_nonce=${BASH_REMATCH[1]}
recovery_helpers=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec systemd-run --user --scope --collect --unit="infra-online-restore-${recovery_nonce}" \
  --property=RuntimeMaxSec=900s --property=TimeoutStopSec=15s --property=KillMode=control-group \
  --property=MemoryMax=1536M --property=CPUQuota=200% --property=TasksMax=192 \
  python3 -B "${recovery_helpers}/evacuation_online_restore.py" \
  --input "$1" --independent-proof "$2" --candidate "$3" --workspace "$4"
