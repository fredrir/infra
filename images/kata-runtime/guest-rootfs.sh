#!/usr/bin/env bash
set -euo pipefail

test "$(id -u)" = 0
for mapping in uid_map gid_map; do
    awk '$1 == 0 && $2 != 0 { root_mapped = 1 } $1 > 0 && $3 >= 65536 { subordinate = 1 } END { exit !(root_mapped && subordinate) }' "/proc/self/$mapping"
    cp "/proc/self/$mapping" "/output/$mapping"
done
test -d /kata/tools/osbuilder/rootfs-builder
test -f /inputs/noble.sources
test -f /inputs/kata-agent
test ! -e /build/rootfs
test -d /output
printf '%s  %s\n' f11b884b7452ece9eab1343f0886cc0918477766ecc7335bda161131a29f4380 /inputs/kata-agent | sha256sum -c -

export ARCH=x86_64 OS_VERSION=noble AGENT_INIT=no AGENT_POLICY=yes SECCOMP=yes
printf '%s  %s\n' 0bb24b3ac02a72f8f7a11cbeab9831d802393860479ef283d8e480d10dee6a19 /kata/src/kata-opa/allow-all.rego | sha256sum -c -
export AGENT_POLICY_FILE=/kata/src/kata-opa/allow-all.rego
export AGENT_SOURCE_BIN=/inputs/kata-agent AGENT_VERSION=4.1.0 LIBC=gnu
export ROOTFS_DIR=/build/rootfs INSIDE_CONTAINER=1 REPO_URL=/inputs/noble.sources
export HOME=/tmp/builder-home XDG_CONFIG_HOME=/tmp/builder-config
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
export PATH=/inputs/bin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p "$HOME" "$XDG_CONFIG_HOME"

ROOTFS_ONLY=yes bash /kata/tools/osbuilder/rootfs-builder/rootfs.sh ubuntu
mkdir -p /output/audit-rootfs/var/lib/dpkg /output/audit-rootfs/usr/lib
dpkg-query --admindir="$ROOTFS_DIR/var/lib/dpkg" -W -f='${binary:Package}\t${Version}\t${Architecture}\n' > /output/packages.tsv
cp "$ROOTFS_DIR/var/lib/dpkg/status" /output/audit-rootfs/var/lib/dpkg/status
cp "$ROOTFS_DIR/usr/lib/os-release" /output/audit-rootfs/usr/lib/os-release
cp /inputs/noble.sources /output/noble.sources
cp /builder-packages.tsv /output/builder-packages.tsv
test -s /output/packages.tsv
python3 - <<'PY'
import json
from pathlib import Path, PurePosixPath
root = Path('/build/rootfs/var/lib/dpkg/info')
result = {}
for source in sorted(root.glob('*.list')):
    entries = []
    for line in source.read_text().splitlines():
        path = PurePosixPath(line)
        if '..' in path.parts or not path.is_absolute():
            raise SystemExit('Invalid package file path')
        if path.as_posix() not in ['/', '/.']:
            entries.append(path.as_posix().lstrip('/'))
    result[source.name.removesuffix('.list')] = sorted(set(entries))
Path('/output/package-files.json').write_text(json.dumps(result, sort_keys=True) + '\n')
PY

ROOTFS_ONLY='' bash /kata/tools/osbuilder/rootfs-builder/rootfs.sh -d
mkdir -p /build/kata-service-source/src
cp -a /kata/src/agent /build/kata-service-source/src/agent
cp /kata/utils.mk /kata/VERSION /build/kata-service-source/
make -C /build/kata-service-source/src/agent install-services INIT=no LIBC=gnu DESTDIR="$ROOTFS_DIR" VERSION=4.1.0 COMMIT=ddcb1ad8d23cbb4323f86c209f132b89592902df
sha256sum "$ROOTFS_DIR/usr/bin/kata-agent" > /output/final-agent.sha256
test -x "$ROOTFS_DIR/lib/systemd/systemd"
test -x "$ROOTFS_DIR/usr/bin/kata-agent"
test -s /build/device-metadata.state
test ! -e "$ROOTFS_DIR/init"
ln -s /lib/systemd/systemd "$ROOTFS_DIR/init"
rm /build/device-metadata.state
(cd "$ROOTFS_DIR/dev" && MAKEDEV -v console tty ttyS null zero fd)
(cd "$ROOTFS_DIR" && find . -print0 | fakeroot --unknown-is-real -i /build/device-metadata.state -- cpio --null -H newc -o | gzip -9) > /output/kata-ubuntu-noble-updated.initrd
sha256sum /output/kata-ubuntu-noble-updated.initrd /output/packages.tsv /output/audit-rootfs/var/lib/dpkg/status > /output/checksums.txt
