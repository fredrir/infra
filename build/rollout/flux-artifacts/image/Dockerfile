FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS builder
WORKDIR /source
RUN curl --fail --silent --show-error --location --retry 2 --max-time 60 https://codeload.github.com/fluxcd/source-watcher/tar.gz/refs/tags/v2.2.4 --output /tmp/source.tar.gz && \
    printf 'f01d3eb03353089627dc384842da2fe7fdfedc05178f25ded8104affb1e90d3f  /tmp/source.tar.gz\n' | sha256sum --check --strict && \
    tar -xzf /tmp/source.tar.gz --strip-components=1 -C /source
COPY build/rollout/flux-artifacts/image/dependencies.patch /tmp/dependencies.patch
RUN git apply --check /tmp/dependencies.patch && git apply /tmp/dependencies.patch
ENV GOTOOLCHAIN=local CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    sha256sum go.mod go.sum > /tmp/module-inputs.sha256 && \
    go mod download && go mod verify && sha256sum --check --strict /tmp/module-inputs.sha256
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false -mod=readonly -ldflags='-s -w -buildid=' -o /out/source-watcher ./cmd/main.go

FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
RUN apk add --no-cache ca-certificates 'libcrypto3>=3.5.8-r0' 'libssl3>=3.5.8-r0' && update-ca-certificates
COPY --from=builder /out/source-watcher /usr/local/bin/source-watcher
COPY --from=builder /source/LICENSE /usr/share/licenses/source-watcher/LICENSE
COPY build/rollout/flux-artifacts/image/source.json /usr/share/source-watcher/source.json
LABEL org.opencontainers.image.title="infra-source-watcher" \
    org.opencontainers.image.version="v2.2.4-infra.1" \
    org.opencontainers.image.source="https://github.com/fredrir/infra" \
    org.opencontainers.image.licenses="Apache-2.0" \
    io.fredrir.upstream.revision="06cb1109bfa7834e8403e2b543d9838e1623f85a"
USER 65534:65534
ENTRYPOINT ["source-watcher"]
