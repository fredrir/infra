#!/bin/sh
set -eu
refuse() {
  echo "refusing to start: $1" >&2
  exit 1
}
[ -s "${WEED_GRPC_CA:-}" ] || refuse "WEED_GRPC_CA is not a readable file"
for component in $WEED_GRPC_COMPONENTS; do
  for kind in CERT KEY; do
    eval "path=\${WEED_GRPC_${component}_${kind}:-}"
    [ -s "$path" ] || refuse "WEED_GRPC_${component}_${kind} is not a readable file"
  done
done
for component in $WEED_GRPC_SERVED; do
  eval "names=\${WEED_GRPC_${component}_ALLOWED_COMMONNAMES:-}"
  [ -n "$names" ] || refuse "WEED_GRPC_${component}_ALLOWED_COMMONNAMES is empty"
done
exec /usr/bin/weed "$@"
