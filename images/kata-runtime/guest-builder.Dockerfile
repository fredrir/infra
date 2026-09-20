FROM docker.io/library/ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254
COPY infra /infra
COPY ca.deb /inputs/ca.deb
COPY guest-builder.sources /etc/apt/sources.list.d/ubuntu.sources
RUN ["/infra", "kata", "prepare-builder"]
