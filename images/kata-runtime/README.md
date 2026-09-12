# Kata runtime

| Input | Value |
|---|---|
| Builder | Native Linux amd64, rootless Podman, two CPUs, 4 GiB memory |
| Host tools | Bash, Git, curl, jq, GNU tar, gzip, zstd, patch, dpkg-deb, debugfs, sha256sum, Python 3.11+ |
| Source and release pins | `ansible/roles/ci_runtime/files/kata-runtime.json` |
| Output | 27 regular files under `/opt/kata` |
| Qualification | Kata Nix sandbox; BuildKit uses gVisor |

```sh
work="$HOME/kata-build-$(date -u +%Y%m%dT%H%M%SZ)"
for component in qemu kernel guest virtiofsd; do
  systemd-run --user --scope -p CPUQuota=200% -p MemoryMax=4G \
    -p MemorySwapMax=0 -p TasksMax=256 -p RuntimeMaxSec=2h \
    bash images/kata-runtime/build.sh "$component" "$work"
done
bash images/kata-runtime/build.sh package "$work"
```

The build uses pinned upstream Kata packaging scripts, one verified already-applied kernel patch omission, locked virtiofsd dependencies, and Ubuntu's signed package snapshot. The rootless guest builder uses fakeroot device metadata; it does not install files or activate runtimes on the builder host.

Rebuilds produce a fresh checksum manifest and require native runtime qualification before publication. Compiler and filesystem timestamps can change archive bytes; the published release checksum identifies the accepted artifact.

```sh
ansible-playbook -i ansible/inventory/production.yml ansible/ci-runtimes.yml \
  --limit fredrir-09 -e ci_kata_enabled=true
```
