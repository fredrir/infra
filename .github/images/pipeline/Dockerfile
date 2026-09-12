FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS tools
RUN GOBIN=/out CGO_ENABLED=0 go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

FROM moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3 AS buildkit
FROM nixos/nix:2.35.2@sha256:7a007c766426c1877758ddc5cb87a965ac131fc78c582ce0083d922d51ae945c AS nix
FROM docker:29-cli@sha256:eccaacfeed644c7de222ff047483568cb988dde95476fbaaf10ea2d04921bb66 AS docker
FROM ghcr.io/sigstore/cosign/cosign:v3.1.3@sha256:9e5c2f2edc34351160407ca3416c61855bdf9403c3c5936e0f0be7fc261611b8 AS cosign
FROM aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969 AS trivy
FROM ghcr.io/astral-sh/uv:0.12.12@sha256:73d2665b478d8fa2de1cf105c6841f8e9cb6b09e568fc7700440c09f8fcd7ac4 AS uv

FROM ubuntu:24.04@sha256:224a1869083a311ef3f13648a154ba79832fbef6364d31493642ca03082da254
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq python3 python3-yaml python3-jsonschema \
    rootlesskit slirp4netns uidmap skopeo xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1001 --create-home --shell /bin/bash builder \
    && printf 'builder:100000:65536\n' > /etc/subuid \
    && printf 'builder:100000:65536\n' > /etc/subgid
COPY --from=tools /out/actionlint /usr/local/bin/actionlint
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildkitd /usr/bin/buildkit-runc /usr/local/bin/
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=cosign /ko-app/cosign /usr/local/bin/cosign
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=nix --chown=1001:1001 /nix /nix
RUN mkdir -p /etc/nix /run/user/1001 \
    && printf 'experimental-features = nix-command flakes\nsandbox = true\nsandbox-fallback = false\nbuild-users-group =\n' > /etc/nix/nix.conf \
    && chown 1001:1001 /run/user/1001
ENV PATH=/nix/var/nix/profiles/default/bin:/usr/local/bin:/usr/bin:/bin
ENV HOME=/home/builder
ENV USER=builder
ENV XDG_RUNTIME_DIR=/run/user/1001
USER 1001:1001
WORKDIR /home/builder
CMD ["sleep", "infinity"]
