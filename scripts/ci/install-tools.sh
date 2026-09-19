#!/usr/bin/env bash
set -euo pipefail
test "$(uname -m)" = x86_64
tools="$RUNNER_TEMP/tools"
install -d -m 700 "$tools"
cd "$tools"

fetch() {
  curl --fail --silent --show-error --location --retry 3 --max-time 120 --output "$1" "$2"
  printf '%s  %s\n' "$3" "$1" | sha256sum --check --strict --quiet
}

install_tool() {
  case "$1" in
    goreleaser)
      fetch goreleaser.tar.gz https://github.com/goreleaser/goreleaser/releases/download/v2.18.2/goreleaser_Linux_x86_64.tar.gz \
        0a96edc9d9bc594e4a41cc4d59467c182062910ab24d9d1f6dd7b667d32606d3
      tar -xzf goreleaser.tar.gz goreleaser
      ;;
    nfpm)
      fetch nfpm.tar.gz https://github.com/goreleaser/nfpm/releases/download/v2.47.0/nfpm_2.47.0_Linux_x86_64.tar.gz \
        0660ca602b2d2d2ae4781a06c692b3eeb9d437ffea05b831d76e41f4a3188783
      tar -xzf nfpm.tar.gz nfpm
      ;;
    git-cliff)
      fetch git-cliff.tar.gz https://github.com/orhun/git-cliff/releases/download/v2.14.1/git-cliff-2.14.1-x86_64-unknown-linux-musl.tar.gz \
        cba6ae86f0a4205784eed8ef049fe53c904138806e33a1bda2e25026b17198eb
      tar -xzf git-cliff.tar.gz --strip-components=1 git-cliff-2.14.1/git-cliff
      ;;
    gh)
      fetch gh.tar.gz https://github.com/cli/cli/releases/download/v2.101.0/gh_2.101.0_linux_amd64.tar.gz \
        9bca2d1c16825f109907a23307628a2f0698fbf99662b73a5cf0b020293072b8
      tar -xzf gh.tar.gz --strip-components=2 gh_2.101.0_linux_amd64/bin/gh
      ;;
    kustomize)
      fetch kustomize.tar.gz https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v5.8.1/kustomize_v5.8.1_linux_amd64.tar.gz \
        029a7f0f4e1932c52a0476cf02a0fd855c0bb85694b82c338fc648dcb53a819d
      tar -xzf kustomize.tar.gz kustomize
      ;;
    trivy)
      fetch trivy.tar.gz https://github.com/aquasecurity/trivy/releases/download/v0.74.0/trivy_0.74.0_Linux-64bit.tar.gz \
        2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a
      tar -xzf trivy.tar.gz trivy
      ;;
    yq)
      fetch yq https://github.com/mikefarah/yq/releases/download/v4.53.6/yq_linux_amd64 \
        c5f056448f973ae7d39b5401949648a78f2dc1947d6a8eb65be60d5c504b9385
      ;;
    cosign)
      fetch cosign https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64 \
        4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71
      ;;
    *)
      printf 'Unknown tool: %s\n' "$1" >&2
      return 1
      ;;
  esac
  rm -f "$1.tar.gz"
  chmod 755 "$1"
}

installs=()
for tool in "$@"; do
  install_tool "$tool" &
  installs+=("$!")
done
for install in "${installs[@]}"; do
  wait "$install"
done
printf '%s\n' "$tools" >> "$GITHUB_PATH"
