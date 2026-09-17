#!/usr/bin/env bash
set -euo pipefail
shared=$(cd "$(dirname "$0")" && pwd)
git tag --list 'v*' > "$RUNNER_TEMP/tags"
next=$(python3 "$shared/next_version.py" Cargo.toml --tags "$RUNNER_TEMP/tags")
if git rev-parse --quiet --verify "refs/tags/v$next" >/dev/null; then
  printf 'v%s already exists\n' "$next"
  exit 0
fi
files=(Cargo.toml CHANGELOG.md)
if [[ -f Cargo.lock ]]; then
  cargo update --workspace --quiet
  files+=(Cargo.lock)
fi
config=cliff.toml
[[ -f "$config" ]] || config="$shared/cliff.toml"
git-cliff --config "$config" --tag "v$next" --output CHANGELOG.md
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git add -- "${files[@]}"
if ! git diff --cached --quiet; then
  git commit --quiet --message "release: v$next"
  git push --quiet origin HEAD:main
fi
git tag --annotate "v$next" --message "v$next"
git push --quiet origin "refs/tags/v$next"
printf 'Tagged v%s\n' "$next"
