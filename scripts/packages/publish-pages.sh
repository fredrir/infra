#!/usr/bin/env bash
set -euo pipefail
site=$(cd "$1" && pwd)
: "${GH_TOKEN:?}" "${GITHUB_REPOSITORY:?}"
megabytes=$(du -sm "$site" | cut -f1)
(( megabytes <= 900 )) || { printf '::error::Package site is %s MB, above the 900 MB Pages budget\n' "$megabytes"; exit 1; }
test -f "$site/CNAME" && test -f "$site/.nojekyll" && test -f "$site/deb/dists/stable/InRelease"
cd "$site"
git init --quiet --initial-branch=gh-pages
git add -A
git -c user.name="fredrir-packages[bot]" -c user.email=packages@fredrir.com commit --quiet --message "Publish packages"
git -c credential.helper='!gh auth git-credential' push --quiet --force "https://github.com/$GITHUB_REPOSITORY.git" HEAD:gh-pages
printf 'Published %s MB to GitHub Pages\n' "$megabytes"
