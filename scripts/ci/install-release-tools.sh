#!/usr/bin/env bash
set -euo pipefail
test "$(uname -m)" = x86_64
tools="$RUNNER_TEMP/release-tools"
mkdir -m700 "$tools"
cd "$tools"
curl --fail --silent --show-error --location --max-time 120 --output gh.tar.gz https://github.com/cli/cli/releases/download/v2.100.0/gh_2.100.0_linux_amd64.tar.gz
curl --fail --silent --show-error --location --max-time 120 --output yq https://github.com/mikefarah/yq/releases/download/v4.53.6/yq_linux_amd64
curl --fail --silent --show-error --location --max-time 120 --output kustomize.tar.gz https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/v5.8.1/kustomize_v5.8.1_linux_amd64.tar.gz
curl --fail --silent --show-error --location --max-time 120 --output trivy.tar.gz https://github.com/aquasecurity/trivy/releases/download/v0.74.0/trivy_0.74.0_Linux-64bit.tar.gz
curl --fail --silent --show-error --location --max-time 120 --output cosign https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64
sha256sum --check <<'SUMS'
e4d4bb4498e8d007abe545b6568926793ace1b6447da598294a610018cb164be  gh.tar.gz
c5f056448f973ae7d39b5401949648a78f2dc1947d6a8eb65be60d5c504b9385  yq
029a7f0f4e1932c52a0476cf02a0fd855c0bb85694b82c338fc648dcb53a819d  kustomize.tar.gz
2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a  trivy.tar.gz
4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71  cosign
SUMS
tar -xzf gh.tar.gz --strip-components=2 gh_2.100.0_linux_amd64/bin/gh
tar -xzf kustomize.tar.gz kustomize
tar -xzf trivy.tar.gz trivy
chmod 755 gh yq kustomize trivy cosign
printf '%s\n' "$tools" >> "$GITHUB_PATH"
