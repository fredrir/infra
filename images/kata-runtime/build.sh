#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
recipe=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
pins="$recipe/../../ansible/roles/ci_runtime/files/kata-runtime.json"
component=${1:?usage: build.sh qemu|kernel|guest|virtiofsd|package ABSOLUTE_WORK_DIRECTORY}
work=${2:?absolute work directory required}
[[ "$work" =~ ^/[a-zA-Z0-9_./-]+$ && "$work" != / ]]
[[ "$(uname -sm)" = 'Linux x86_64' ]]
[[ "$(podman info --format '{{.Host.Security.Rootless}}')" = true ]]
case "$component" in qemu|kernel|guest|virtiofsd|package) ;; *) exit 1;; esac
mkdir -p "$work"
if [[ "$component" = package ]]; then
    exec python3 -I -B "$recipe/package.py" "$work"
fi
mkdir "$work/$component"
part="$work/$component"
mkdir "$part/build" "$part/output"
umask 077
printf '{"auths":{}}\n' > "$part/auth.json"
export REGISTRY_AUTH_FILE="$part/auth.json"
get() { jq -er ".components.$1" "$pins"; }
fetch() {
    curl --disable --fail --location --connect-timeout 15 --max-time 180 "$1" --output "$2"
    printf '%s  %s\n' "$3" "$2" | sha256sum -c -
}
clone() {
    git -c credential.helper= clone --depth 1 --branch "$2" "$1" "$3"
    test "$(git -C "$3" rev-parse HEAD)" = "$4"
}
clone https://github.com/kata-containers/kata-containers.git "$(get kata.version)" "$part/source" "$(get kata.sourceRevision)"
cpus=$(python3 -I -c 'import os; print(",".join(map(str,sorted(os.sched_getaffinity(0))[:2])))')
name="kata-build-$component-$$"
trap 'podman rm --force --ignore "$name" >/dev/null' EXIT
run=(podman run --rm --name "$name" --timeout 7200 --cpuset-cpus "$cpus" --cpus 2 --memory 4g --memory-swap 4g --pids-limit 256 --http-proxy=false)
run+=(--env GIT_CONFIG_NOSYSTEM=1 --env GIT_CONFIG_GLOBAL=/dev/null --env GIT_TERMINAL_PROMPT=0 --env OMP_NUM_THREADS=2 --env OMP_THREAD_LIMIT=2 --env HOME=/tmp/build-home --env XDG_CONFIG_HOME=/tmp/build-config)
builder=$(get "$component.builder")
case "$component" in
qemu)
    clone "$(get qemu.repository)" "$(get qemu.tag)" "$part/qemu" "$(get qemu.sourceRevision)"
    marker="$part/source/tools/packaging/qemu/patches/tag_patches/$(get qemu.tag)/no_patches.txt"
    mkdir -p "$(dirname "$marker")"
    touch "$marker"
    "${run[@]}" --userns keep-id --user "$(id -u):$(id -g)" \
      -v "$part/source:$part/source:ro" -v "$part/qemu:$part/qemu:ro" -v "$part/output:/share" \
      --env "QEMU_REPO=file://$part/qemu" --env "QEMU_VERSION_NUM=$(get qemu.tag)" \
      --env HYPERVISOR_NAME=kata-qemu --env PKGVERSION=kata-static --env PREFIX=/opt/kata \
      --env QEMU_DESTDIR=/tmp/qemu-static --env QEMU_TARBALL=kata-static-qemu.tar.gz --env ARCH=x86_64 \
      "$builder" bash "$part/source/tools/packaging/static-build/qemu/build-qemu.sh"
    ;;
kernel)
    version=$(get kernel.version)
    tarball="$part/build/linux-$version.tar.xz"
    fetch "$(get kernel.url)" "$tarball" "$(get kernel.sha256)"
    printf '%s  %s\n' "$(get kernel.sha256)" "linux-$version.tar.xz" > "$tarball.sha256"
    mkdir "$part/patch-check"
    tar -xJf "$tarball" -C "$part/patch-check" --strip-components=1 "linux-$version/fs/dax.c" "linux-$version/fs/fuse/file.c"
    while IFS=$'\t' read -r filename digest action; do
        patchfile="$part/source/tools/packaging/kernel/patches/6.18.x/$filename"
        printf '%s  %s\n' "$digest" "$patchfile" | sha256sum -c -
        direction=--forward
        if [[ "$action" = omit-already-applied ]]; then direction=--reverse; fi
        patch --dry-run --batch "$direction" -p1 -d "$part/patch-check" -i "$patchfile"
        if [[ "$action" = omit-already-applied ]]; then rm "$patchfile"; fi
    done < <(jq -r '.components.kernel.packagingPatches[]|[.path,.sha256,.action]|@tsv' "$pins")
    for phase in setup build install; do
        "${run[@]}" --userns keep-id --user "$(id -u):$(id -g)" \
          -v "$part/source:$part/source:ro" -v "$part/build:$part/build" -v "$part/output:$part/output" \
          --workdir "$part/build" --env "DESTDIR=$part/output" --env PREFIX=/opt/kata --env KERNEL_DEBUG_ENABLED=no \
          "$builder" bash "$part/source/tools/packaging/kernel/build-kernel.sh" -v "$version" -a x86_64 -t qemu "$phase"
    done
    ;;
virtiofsd)
    clone "$(get virtiofsd.repository)" "v$(get virtiofsd.version)" "$part/virtiofsd" "$(get virtiofsd.sourceRevision)"
    printf '%s  %s\n' "$(get virtiofsd.cargoLockSha256)" "$part/virtiofsd/Cargo.lock" | sha256sum -c -
    "${run[@]}" -v "$part/virtiofsd:/source:ro" -v "$recipe/virtiofsd.sh:/inputs/build.sh:ro" \
      -v "$part/build:/build" -v "$part/output:/output" "$builder" sh /inputs/build.sh
    ;;
guest)
    mkdir "$part/context" "$part/inputs" "$part/inputs/bin"
    fetch https://snapshot.ubuntu.com/ubuntu/20260912T000000Z/pool/main/c/ca-certificates/ca-certificates_20240203_all.deb \
      "$part/ca.deb" 641de77d8f142cfd62a1a6f964ba67b20754d3337c480efb529d086075a06c9a
    fetch https://snapshot.ubuntu.com/ubuntu/20260912T000000Z/pool/universe/m/makedev/makedev_2.3.1-97.1_all.deb \
      "$part/makedev.deb" 90363b8e3085545da5de8b8daf603f84d64e3d407197340100ed317eda616a50
    dpkg-deb -x "$part/ca.deb" "$part/ca"
    dpkg-deb -x "$part/makedev.deb" "$part/makedev"
    cat "$part/ca/usr/share/ca-certificates/mozilla/"*.crt > "$part/context/ca-certificates.crt"
    cp "$part/makedev/usr/sbin/MAKEDEV" "$part/context/MAKEDEV"
    printf '%s  %s\n' "$(get guest.bootstrapCaBundleSha256)" "$part/context/ca-certificates.crt" "$(get guest.makedevScriptSha256)" "$part/context/MAKEDEV" | sha256sum -c -
    fetch "$(get kata.url)" "$part/kata.tar.zst" "$(get kata.sha256)"
    tar --zstd -xOf "$part/kata.tar.zst" ./opt/kata/share/kata-containers/kata-ubuntu-noble.image > "$part/guest.ext4"
    debugfs -R "dump /usr/bin/kata-agent $part/inputs/kata-agent" "$part/guest.ext4"
    chmod 755 "$part/inputs/kata-agent"
    printf '%s  %s\n' "$(get guest.agentSha256)" "$part/inputs/kata-agent" | sha256sum -c -
    cp "$recipe/guest-builder.Dockerfile" "$recipe/guest-builder.sources" "$part/context/"
    cp "$recipe/guest-rootfs.sh" "$recipe/noble.sources" "$part/inputs/"
    cp "$recipe/makedev-metadata.sh" "$part/inputs/bin/MAKEDEV"
    chmod 755 "$part/inputs/bin/MAKEDEV" "$part/context/MAKEDEV"
    podman build --http-proxy=false --jobs 1 --force-rm --isolation oci --network private --cpuset-cpus "$cpus" \
      --cpu-period 100000 --cpu-quota 200000 --memory 4g --memory-swap 4g \
      --iidfile "$part/builder.id" -f "$part/context/guest-builder.Dockerfile" "$part/context"
    "${run[@]}" --cap-add SYS_ADMIN -v "$part/source:/kata:ro" -v "$part/inputs:/inputs:ro" \
      -v "$part/build:/build" -v "$part/output:/output" "$(cat "$part/builder.id")" bash /inputs/guest-rootfs.sh
    ;;
esac
