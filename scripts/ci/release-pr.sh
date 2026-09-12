#!/usr/bin/env bash
set -euo pipefail
[[ "$SOURCE_REPOSITORY_ID" =~ ^[0-9]+$ ]]
[[ "$GITHUB_REPOSITORY" =~ ^fredrir/[A-Za-z0-9_.-]+$ ]]
[[ "$GITHUB_SHA" =~ ^[a-f0-9]{40}$ ]]
[[ "$IMAGE" =~ ^ghcr\.io/fredrir/[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$ ]]
[[ "$GITHUB_RUN_ID" =~ ^[0-9]+$ && "$GITHUB_RUN_ATTEMPT" =~ ^[0-9]+$ ]]
cd -P "$1"
test "$(git config --get remote.origin.url)" = https://github.com/fredrir/infra
test -z "$(git status --porcelain)"
mapping=".github/deployments/$SOURCE_REPOSITORY_ID.yaml"
test "$(yq -r '.repository' "$mapping")" = "$GITHUB_REPOSITORY"
export IMAGE_NAME="${IMAGE%@*}"
path=$(yq -er '.images[strenv(IMAGE_NAME)].path' "$mapping")
[[ "$path" =~ ^platform/projects/[a-z][a-z0-9-]*$ ]]
test "$(realpath "$path")" = "$PWD/$path"
mode=$(yq -er '.images[strenv(IMAGE_NAME)].mode' "$mapping")
case "$mode" in
  kustomize)
    (cd "$path" && kustomize edit set image "$IMAGE_NAME=$IMAGE")
    ;;
  helmrelease)
    export WORKLOAD
    WORKLOAD=$(yq -er '.images[strenv(IMAGE_NAME)].workload' "$mapping")
    [[ "$WORKLOAD" =~ ^[a-z][a-z0-9-]*$ ]]
    yq -e '.kind == "HelmRelease" and (.spec.values.workloads[strenv(WORKLOAD)] != null)' "$path/release.yaml" > /dev/null
    yq -i '.spec.values.workloads[strenv(WORKLOAD)].image = strenv(IMAGE) | .spec.values.workloads[strenv(WORKLOAD)].sourceRevision = strenv(GITHUB_SHA)' "$path/release.yaml"
    ;;
  *) exit 1 ;;
esac
kustomize build "$path" > /dev/null
git diff --check
if git diff --quiet; then exit 0; fi
while IFS= read -r changed; do
  test "$changed" = "$path/kustomization.yaml" || test "$changed" = "$path/release.yaml"
done < <(git diff --name-only)
branch="deploy/$SOURCE_REPOSITORY_ID/$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-${IMAGE_NAME##*/}"
git switch -c "$branch"
git add -- "$path"
git -c user.name='infra-release' -c user.email='infra-release@users.noreply.github.com' commit -m "Deploy ${IMAGE_NAME##*/} ${GITHUB_SHA:0:12}"
git -c credential.helper='!gh auth git-credential' push origin "HEAD:refs/heads/$branch"
body="$RUNNER_TEMP/deployment-pr.txt"
printf 'Deploy %s from [%s](https://github.com/%s/commit/%s).\n\nNative signature verification bound the protected main source revision and pinned build workflow before this update.\n' "$IMAGE" "$GITHUB_SHA" "$GITHUB_REPOSITORY" "$GITHUB_SHA" > "$body"
gh pr create --repo fredrir/infra --base main --head "$branch" --title "Deploy ${IMAGE_NAME##*/} ${GITHUB_SHA:0:12}" --body-file "$body"
