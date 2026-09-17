#!/bin/sh
set -eu
format=$1
package=$2
binary=$3
case "$format" in
  deb)
    echo "deb [signed-by=/repo/keys/fredrir.asc] file:/repo/deb stable main" > /etc/apt/sources.list.d/fredrir.list
    apt-get update -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list -o Dir::Etc::sourceparts=-
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/fredrir.list -o Dir::Etc::sourceparts=- "$package"
    ;;
  rpm)
    rpm --import /repo/keys/fredrir.asc
    if command -v zypper >/dev/null; then
      zypper --non-interactive addrepo --check --gpgcheck --refresh 'file:///repo/rpm/$basearch' fredrir
      zypper --non-interactive install --repo fredrir "$package"
    else
      printf '[fredrir]\nname=fredrir\nbaseurl=file:///repo/rpm/$basearch\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///repo/keys/fredrir.asc\n' \
        > /etc/yum.repos.d/fredrir.repo
      dnf install -y --repo=fredrir "$package"
    fi
    ;;
  apk)
    cp /repo/keys/fredrir.rsa.pub /etc/apk/keys/
    apk add --no-cache --no-network --repository /repo/apk "$package"
    ;;
  *)
    echo "unknown format $format" >&2
    exit 1
    ;;
esac
"$binary" --version
