#!/usr/bin/env bash
set -euo pipefail
[[ "$SOURCE_REPOSITORY_ID" =~ ^[0-9]+$ ]]
[[ "$SOURCE_REVISION" =~ ^[a-f0-9]{40}$ ]]
[[ "$IMAGE_NAME" =~ ^ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*$ ]]
[[ "$IMAGE_DIGEST" =~ ^sha256:[a-f0-9]{64}$ ]]
: "${DEPLOY_TOKEN:?}"
export IMAGE_NAME SOURCE_REVISION IMAGE="$IMAGE_NAME@$IMAGE_DIGEST"
workflow=fredrir/infra/.github/workflows/build-image.yml
cd -P "$1"
test "$(git config --get remote.origin.url)" = https://github.com/fredrir/infra
test -z "$(git status --porcelain)"
mapping=".github/deployments/$SOURCE_REPOSITORY_ID.yaml"
repository=$(yq -er '.repository' "$mapping")
[[ "$repository" =~ ^fredrir/[A-Za-z0-9_.-]+$ ]]
path=$(yq -er '.images[strenv(IMAGE_NAME)].path' "$mapping")
[[ "$path" =~ ^platform/projects/[a-z][a-z0-9-]*$ ]]
test "$(realpath "$path")" = "$PWD/$path"
mode=$(yq -er '.images[strenv(IMAGE_NAME)].mode' "$mapping")
workflow_revision=$(yq -er '.claim_pattern.job_workflow_sha' ".github/chainguard/deploy-$SOURCE_REPOSITORY_ID.sts.yaml")
workflow_revision=${workflow_revision#^}
workflow_revision=${workflow_revision%\$}
[[ "$workflow_revision" =~ ^[a-f0-9]{40}$ ]]
case "$(yq -er '.visibility' "$mapping")" in
  public)
    gh attestation verify "oci://$IMAGE" --repo "$repository" --signer-workflow "$workflow" \
      --signer-digest "$workflow_revision" --source-ref refs/heads/main --source-digest "$SOURCE_REVISION"
    ;;
  private)
    cosign verify --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --certificate-identity "https://github.com/$workflow@$workflow_revision" \
      --certificate-github-workflow-repository "$repository" \
      --certificate-github-workflow-sha "$SOURCE_REVISION" \
      --certificate-github-workflow-ref refs/heads/main \
      --certificate-github-workflow-trigger push \
      --annotations "source-repository=$repository" \
      --annotations "source-revision=$SOURCE_REVISION" \
      --annotations "workflow-revision=$workflow_revision" "$IMAGE" > /dev/null
    ;;
  *) exit 1 ;;
esac
case "$mode" in
  kustomize)
    pinned=0
    while IFS= read -r kustomization; do
      yq -e '.images // [] | .[] | select(.name == strenv(IMAGE_NAME))' "$kustomization" > /dev/null || continue
      (cd "$(dirname "$kustomization")" && kustomize edit set image "$IMAGE_NAME=$IMAGE")
      kustomize build "$(dirname "$kustomization")" > /dev/null
      pinned=$((pinned + 1))
    done < <(find "$path" -name kustomization.yaml | sort)
    test "$pinned" -gt 0
    ;;
  helmrelease)
    export WORKLOAD
    WORKLOAD=$(yq -er '.images[strenv(IMAGE_NAME)].workload' "$mapping")
    [[ "$WORKLOAD" =~ ^[a-z][a-z0-9-]*$ ]]
    yq -e '.kind == "HelmRelease" and (.spec.values.workloads[strenv(WORKLOAD)] != null)' "$path/release.yaml" > /dev/null
    yq -i '.spec.values.workloads[strenv(WORKLOAD)].image = strenv(IMAGE) | .spec.values.workloads[strenv(WORKLOAD)].sourceRevision = strenv(SOURCE_REVISION)' "$path/release.yaml"
    ;;
  *) exit 1 ;;
esac
kustomize build "$path" > /dev/null
git diff --check
if git diff --quiet; then
  printf '%s is already deployed\n' "$IMAGE"
  exit 0
fi
while IFS= read -r changed; do
  [[ "$changed" =~ ^$path(/[a-z][a-z0-9-]*)*/kustomization\.yaml$ ]] || test "$changed" = "$path/release.yaml"
done < <(git diff --name-only)
git add -- "$path"
git -c user.name='infra-release' -c user.email='infra-release@users.noreply.github.com' commit --quiet \
  -m "Deploy ${IMAGE_NAME##*/} ${SOURCE_REVISION:0:12}" \
  -m "Deploy $IMAGE from https://github.com/$repository/commit/$SOURCE_REVISION with verified provenance."
publish() { GH_TOKEN="$DEPLOY_TOKEN" git -c credential.helper='!gh auth git-credential' "$@"; }
for attempt in 1 2 3; do
  if publish push --quiet origin HEAD:main; then
    printf 'Deployed %s\n' "$IMAGE"
    exit 0
  fi
  publish fetch --quiet origin main
  git -c user.name='infra-release' -c user.email='infra-release@users.noreply.github.com' rebase --quiet FETCH_HEAD
done
exit 1
