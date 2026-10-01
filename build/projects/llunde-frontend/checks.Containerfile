FROM docker.io/oven/bun:1.4.2-slim@sha256:cb3bbbb08e13a4a2ff400f24c7a2a1d5efa83f6ef8544d52d95a519631e2fc61 AS web
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile
COPY . .
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project llunde-frontend --suite web
