#!/usr/bin/env bash
set -euo pipefail
present() { git cat-file -e "$BEFORE^{commit}" 2> /dev/null; }
if [[ "${BEFORE:-}" =~ ^[a-f0-9]{40}$ ]] && { present || git fetch --quiet --depth=1 origin "$BEFORE"; } && present; then
  files=$(git diff --name-only "$BEFORE" HEAD)
else
  files=$(git ls-files)
fi
changed() { grep -Eq "$1" <<< "$files"; }
logs=$(mktemp -d)
suites=()
suite() {
  if [[ ${#suites[@]} -eq 0 ]]; then uv sync --frozen --group ci --quiet; fi
  LD_LIBRARY_PATH=${CHECK_LIBRARY_PATH:-${LD_LIBRARY_PATH:-}} uv run --no-sync python -m unittest discover -s "$1" > "$logs/${1//\//-}" 2>&1 &
  suites+=("$!:$1")
}
report() {
  local failed=0 entry name
  for entry in "${suites[@]}"; do
    name=${entry#*:}
    wait "${entry%%:*}" || failed=1
    printf '::group::%s\n' "$name"
    cat "$logs/${name//\//-}"
    printf '::endgroup::\n'
  done
  return "$failed"
}

python='^(pyproject\.toml|uv\.lock)$'
if changed "$python|^(scripts/(ci|packages)/|\.github/|tests/(ci|fixtures|golden)/|images/|platform/components/(policy|runners)/)"; then suite tests/ci; fi
if changed "$python|^(tools/|tests/infra/|platform/components/runners/|charts/)"; then suite tests/infra; fi
if changed "$python|^(scripts/operations/|tests/operations/|platform/components/)"; then suite tests/operations; fi
if changed '^ansible/'; then ansible-playbook -i localhost, ansible/site.yml --syntax-check; fi
if changed '^\.github/'; then actionlint; fi
if changed '^(platform|charts)/'; then
  for directory in platform/clusters/production platform/components/* platform/projects/*; do
    if [[ -f "$directory/kustomization.yaml" ]]; then kubectl kustomize "$directory" > /dev/null; fi
  done
fi
if changed '^(charts/|tests/infra/fixtures/)'; then helm lint charts/project --strict -f tests/infra/fixtures/values.yaml; fi
if changed '^tofu/'; then
  tofu -chdir=tofu fmt -check -recursive
  tofu -chdir=tofu init -backend=false -lockfile=readonly -input=false
  tofu -chdir=tofu validate
fi
if changed '^flake\.(nix|lock)$'; then printf 'toolchain=true\n' >> "$GITHUB_OUTPUT"; fi
report
