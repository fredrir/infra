#!/usr/bin/env bash
set -euo pipefail

deny() {
  printf '::error::Runner pool %s refuses %s on %s: %s\n' "${CI_POOL:-unset}" "${GITHUB_EVENT_NAME:-unset}" "${GITHUB_REF:-unset}" "$1" >&2
  exit 1
}

protected_main() {
  [[ "${GITHUB_REF:-}" == refs/heads/main && "${GITHUB_REF_PROTECTED:-}" == true ]]
}

[[ "${GITHUB_REPOSITORY_OWNER_ID:-}" == 114402558 ]] || deny "foreign repository owner"

case "${CI_POOL:-}" in
  pr)
    [[ "${GITHUB_EVENT_NAME:-}" == pull_request ]] || deny "only pull_request events"
    head=$(jq -er '.pull_request.head.repo.full_name' "${GITHUB_EVENT_PATH:?}") || deny "unreadable pull request head"
    [[ "$head" == "${GITHUB_REPOSITORY:?}" ]] || deny "fork pull requests never run"
    ;;
  main)
    [[ "${GITHUB_EVENT_NAME:-}" == push || "${GITHUB_EVENT_NAME:-}" == workflow_dispatch ]] || deny "only push or workflow_dispatch"
    protected_main || deny "only the protected main branch"
    ;;
  release)
    case "${GITHUB_EVENT_NAME:-}:${GITHUB_REF:-}" in
      push:refs/tags/v*) [[ "${GITHUB_REF_PROTECTED:-}" == true ]] || deny "only protected release tags" ;;
      workflow_dispatch:*) protected_main || deny "dry runs only from the protected main branch" ;;
      *) deny "only release tags or main dry runs" ;;
    esac
    ;;
  *)
    deny "unknown pool"
    ;;
esac
