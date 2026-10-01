FROM ghcr.io/fredrir/infra-runner-rust@sha256:1df48e3f7153dd7acd547791b102125de01492ba7b2b18088b560f3f8ac84267 AS rust-deep
USER root
ENV CARGO_BUILD_JOBS=2 CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=0 CARGO_PROFILE_TEST_DEBUG=0
RUN apt-get update && apt-get install -y --no-install-recommends neovim && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    --mount=type=cache,id=nsql-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=nsql-cargo-target,target=/app/target,sharing=locked \
    infra ci project check --project nsql --suite rust-deep
