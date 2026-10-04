# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
ARG PYTHON_IMAGE=docker.io/library/python:3.14.7-slim-bookworm@sha256:82bc3c539b8813ada9d68c63b40158fa002f7f33de9bf3312a3dfdc0620dff56

FROM ghcr.io/astral-sh/uv:0.12.20@sha256:100047e74f30778ab704942321a09750d6158739573ff58bf3924085cc6cd2d8 AS uv

FROM ${PYTHON_IMAGE} AS base
WORKDIR /app
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

RUN sed -i 's|http://|https://|g' /etc/apt/sources.list.d/debian.sources \
  && apt-get update \
  && apt-get upgrade -y \
  && apt-get install -y --no-install-recommends \
       chromium \
       poppler-utils \
       libreoffice-core \
       libreoffice-writer \
       libgl1 \
       libgomp1 \
       libglib2.0-0 \
       libsm6 \
       libxext6 \
       curl \
  && rm -rf /var/lib/apt/lists/*

FROM base AS runtime-lock
WORKDIR /lock
COPY pyproject.toml uv.lock ./
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
  UV_NO_CONFIG=1 uv export --frozen --no-dev --no-emit-project --format pylock.toml --no-header --quiet --output-file /pylock.toml

FROM base AS deps
COPY --from=runtime-lock /pylock.toml /usr/local/share/pyparser/pylock.toml
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
  --mount=type=cache,id=pyparser-uv,target=/root/.cache/uv \
  pip install --upgrade pip "setuptools>=84.0.0" "wheel>=0.48.0" \
  && uv pip install --system --require-hashes --no-deps -r /usr/local/share/pyparser/pylock.toml

FROM docker.io/library/postgres:17-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652 AS postgres
FROM deps AS python
COPY --from=postgres /usr/lib/postgresql/17 /usr/lib/postgresql/17
COPY --from=postgres /usr/share/postgresql /usr/share/postgresql
COPY --from=postgres /usr/lib/x86_64-linux-gnu/libpq.so.5* /usr/lib/x86_64-linux-gnu/
RUN apt-get update && apt-get install -y --no-install-recommends libldap-2.5-0 libicu72 liblz4-1 libzstd1 libreadline8 libxml2 \
    && rm -rf /var/lib/apt/lists/* && useradd --system --create-home postgres
ENV PATH=/usr/lib/postgresql/17/bin:$PATH PYPARSER_ENV=development
COPY . /app
WORKDIR /app
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
    --mount=type=cache,id=pyparser-uv,target=/root/.cache/uv \
    uv export --frozen --group dev --no-emit-project --format pylock.toml --output-file /tmp/pylock.dev.toml \
    && uv pip install --system --require-hashes --no-deps -r /tmp/pylock.dev.toml \
    && uv pip install --system --no-deps --no-build-isolation -e .
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project llunde-pyparser --suite python

FROM docker.io/oven/bun:1.4.2-slim@sha256:cb3bbbb08e13a4a2ff400f24c7a2a1d5efa83f6ef8544d52d95a519631e2fc61 AS ui
WORKDIR /app
COPY review-ui /app/review-ui
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project llunde-pyparser --suite ui
