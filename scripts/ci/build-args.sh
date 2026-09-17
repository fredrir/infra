#!/usr/bin/env bash
set -euo pipefail
[[ "$GITHUB_SHA" =~ ^[a-f0-9]{40}$ ]]
build_args=(--opt "build-arg:GIT_SHA=$GITHUB_SHA" --opt "build-arg:REVISION=$GITHUB_SHA")
seen=$'\nGIT_SHA\nREVISION\n'
while IFS= read -r argument; do
    [[ -n "$argument" ]] || continue
    [[ "$argument" = *=* && "$argument" != *$'\r'* ]]
    name=${argument%%=*}
    [[ "$name" =~ ^(VITE_|PUBLIC_|NEXT_PUBLIC_)[A-Z0-9_]+$ || "$name" = APP_VERSION ]]
    [[ "$seen" != *$'\n'"$name"$'\n'* ]]
    seen+="$name"$'\n'
    build_args+=(--opt "build-arg:$argument")
done <<< "${BUILD_ARGS:-}"
