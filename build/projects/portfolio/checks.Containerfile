FROM docker.io/library/rust@sha256:2775a09d208ff0d7c1f50490c45b62db929e87ba1dcbc3f2132ac71a704bcdd3 AS rust
FROM docker.io/oven/bun@sha256:ec06c3b6cea04192ae6770c434f668ca41d343ad19fa6472216c7b48be39c598 AS bun
FROM docker.io/library/postgres:17-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652 AS rust-base
COPY --from=rust /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i 's|http://|https://|g' /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/pgdg.list \
    && apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates git pkg-config libssl-dev && rm -rf /var/lib/apt/lists/*
COPY --from=rust /usr/local/cargo /usr/local/cargo
COPY --from=rust /usr/local/rustup /usr/local/rustup
ENV CARGO_HOME=/usr/local/cargo RUSTUP_HOME=/usr/local/rustup PATH=/usr/local/cargo/bin:$PATH \
    CARGO_BUILD_JOBS=2 CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=0 CARGO_PROFILE_TEST_DEBUG=0
RUN rustup component add rustfmt clippy
WORKDIR /app

FROM rust-base AS source
COPY . .

FROM source AS rust-tests
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    --mount=type=cache,id=portfolio-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=portfolio-cargo-target,target=/app/target,sharing=locked \
    infra ci project check --project portfolio --suite rust

FROM bun AS web-tests
WORKDIR /app
COPY package.json bun.lock ./
COPY apps/web/package.json apps/web/package.json
COPY apps/edge/package.json apps/edge/package.json
COPY packages/api-client/package.json packages/api-client/package.json
RUN bun install --frozen-lockfile
COPY . .
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project portfolio --suite web

FROM rust-base AS quality-tools
RUN cargo install --locked cargo-audit --version 0.22.2
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun

FROM quality-tools AS quality-tests
COPY . .
RUN bun install --frozen-lockfile
ARG CI_REVISION
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    --mount=type=cache,id=portfolio-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=portfolio-cargo-target,target=/app/target,sharing=locked \
    infra ci project check --project portfolio --suite quality

FROM rust-base AS mutation-tools
RUN cargo install --locked cargo-mutants --version 27.1.0
FROM mutation-tools AS mutation-test
COPY . .
ARG CI_REVISION
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    --mount=type=cache,id=portfolio-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=portfolio-cargo-target,target=/app/target,sharing=locked \
    infra ci project check --project portfolio --suite mutation --report-dir /reports --capture-result
FROM scratch AS mutation-results
COPY --from=mutation-test /reports/ /

FROM rust-base AS fuzz-tools
RUN apt-get update && apt-get install -y --no-install-recommends clang && rm -rf /var/lib/apt/lists/*
RUN rustup toolchain install nightly-2026-09-01 --profile minimal
RUN cargo install --locked cargo-fuzz --version 0.13.2
FROM fuzz-tools AS fuzz-test
COPY . .
ARG CI_REVISION
ARG PUBLIC_FUZZ_SUITE
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project portfolio --suite "$PUBLIC_FUZZ_SUITE" --report-dir /reports --capture-result
FROM scratch AS fuzz-results
COPY --from=fuzz-test /reports/ /

FROM bun AS security
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project portfolio --suite security
