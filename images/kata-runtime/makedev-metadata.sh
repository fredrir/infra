#!/bin/sh
set -eu
test "$PWD" = /build/rootfs/dev
test "$*" = "-v console tty ttyS null zero fd"
sha256sum /sbin/MAKEDEV > /output/makedev-script.sha256
fakeroot --unknown-is-real -s /build/device-metadata.state -- /sbin/MAKEDEV "$@" > /output/makedev.log 2>&1
cat /output/makedev.log
if grep -Eqi "failed|warning:|don.t know how" /output/makedev.log; then
    exit 1
fi
python3 - <<'PYTHON'
import json
from pathlib import Path
import shutil
root = Path('/build/rootfs/dev')
allowed = {'console', 'tty', 'null', 'zero', 'fd', 'stdin', 'stdout', 'stderr'} | {f'ttyS{i}' for i in range(4)}
removed = []
for entry in sorted(root.iterdir()):
    if entry.name not in allowed:
        removed.append(entry.name)
        if entry.is_dir() and not entry.is_symlink():
            shutil.rmtree(entry)
        else:
            entry.unlink()
Path('/output/device-pruning.json').write_text(json.dumps(removed) + '\n')
PYTHON
