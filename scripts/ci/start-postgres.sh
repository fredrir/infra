#!/usr/bin/env bash
set -euo pipefail
[[ "${URL_ENV:?}" =~ ^[A-Z][A-Z0-9_]*$ ]] || { printf '::error::postgres-url-env must be an environment variable name\n'; exit 1; }
data=$(mktemp -d /tmp/postgres.XXXXXX)
chown postgres:postgres "$data"
as_postgres() { setpriv --reuid=postgres --regid=postgres --init-groups -- "$@"; }
as_postgres /usr/lib/postgresql/16/bin/initdb --pgdata="$data/cluster" --username=postgres --auth=trust >/dev/null
as_postgres /usr/lib/postgresql/16/bin/pg_ctl --pgdata="$data/cluster" --wait --log="$data/server.log" \
  --options="-c listen_addresses=127.0.0.1 -c port=5432 -c unix_socket_directories=$data -c fsync=off" start
printf '%s=postgres://postgres@127.0.0.1:5432/postgres\n' "$URL_ENV" >> "$GITHUB_ENV"
