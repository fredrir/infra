import hashlib
import io
import json
import subprocess
import sys
import tarfile
from pathlib import Path

recipe = Path(__file__).resolve().parent
pins = json.loads((recipe / '../../ansible/roles/ci_runtime/files/kata-runtime.json').read_text())
work = Path(sys.argv[1]).resolve(strict=True)
output = work / 'kata-runtime-amd64.tar'
files = {}


def add(name, data, mode):
    if name not in pins['files'] or name in files:
        raise ValueError('Unexpected or duplicate runtime path')
    files[name] = (data, mode)


with tarfile.open(work / 'qemu/output/kata-static-qemu.tar.gz') as archive:
    for member in archive:
        if member.isdir():
            continue
        if not member.isfile():
            raise ValueError('Regular runtime files required')
        add(member.name.removeprefix('./'), archive.extractfile(member).read(), member.mode)

release = work / 'guest/kata.tar.zst'
with release.open('rb') as stream:
    if hashlib.file_digest(stream, 'sha256').hexdigest() != pins['components']['kata']['sha256']:
        raise ValueError('Upstream release checksum mismatch')
name = 'opt/kata/runtime-rs/bin/containerd-shim-kata-v2'
data = subprocess.check_output(['tar', '--zstd', '-xOf', str(release), './' + name])
if hashlib.sha256(data).hexdigest() != pins['files'][name]:
    raise ValueError('Upstream shim checksum mismatch')
add(name, data, 0o755)
for name, source, mode in [
    ('opt/kata/libexec/virtiofsd', work / 'virtiofsd/output/virtiofsd', 0o755),
    ('opt/kata/share/kata-containers/vmlinux-6.18.51-202', work / 'kernel/output/opt/kata/share/kata-containers/vmlinux-6.18.51-202', 0o644),
    ('opt/kata/share/kata-containers/guest.initrd', work / 'guest/output/kata-ubuntu-noble-updated.initrd', 0o644),
    ('opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml', recipe / 'configuration.toml', 0o644),
]:
    add(name, source.read_bytes(), mode)
if files.keys() != pins['files'].keys():
    raise ValueError('Incomplete runtime closure')
with output.open('xb') as stream, tarfile.open(fileobj=stream, mode='w') as archive:
    for name, (data, mode) in sorted(files.items()):
        member = tarfile.TarInfo(name)
        member.size, member.mode = len(data), mode
        archive.addfile(member, io.BytesIO(data))
manifest = {
    'archive': output.name,
    'sha256': hashlib.sha256(output.read_bytes()).hexdigest(),
    'files': {name: hashlib.sha256(data).hexdigest() for name, (data, _) in files.items()},
    'components': pins['components'],
    'nativeQualificationRequired': True,
}
output.with_suffix('.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(output)
