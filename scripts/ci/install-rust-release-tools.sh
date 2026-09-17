#!/usr/bin/env bash
set -euo pipefail
test "$(uname -m)" = x86_64
tools="$RUNNER_TEMP/rust-release-tools"
install -d -m 700 "$tools"
cd "$tools"

fetch() {
  curl --fail --silent --show-error --location --retry 3 --max-time 120 --output "$1" "$2"
  printf '%s  %s\n' "$3" "$1" | sha256sum --check --strict --quiet
}

for tool in "$@"; do
  case "$tool" in
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
    *)
      printf 'Unknown release tool: %s\n' "$tool" >&2
      exit 1
      ;;
  esac
  rm -f "$tool.tar.gz"
  chmod 755 "$tool"
done
printf '%s\n' "$tools" >> "$GITHUB_PATH"
