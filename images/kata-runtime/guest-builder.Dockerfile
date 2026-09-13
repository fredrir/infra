FROM docker.io/library/ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY guest-builder.sources /etc/apt/sources.list.d/ubuntu.sources
RUN printf 'APT::Update::Error-Mode "any";\n' > /etc/apt/apt.conf.d/99-fail-closed && apt-get update && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends binutils ca-certificates cpio curl e2fsprogs fakeroot file git gnupg jq make mmdebstrap python3 sudo ubuntu-keyring xz-utils zstd && dpkg-query -W -f='${binary:Package}\t${Version}\t${Architecture}\n' > /builder-packages.tsv
COPY MAKEDEV /usr/sbin/MAKEDEV
