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

FROM deps AS models
ENV DOCLING_ARTIFACTS_PATH=/opt/docling-models
RUN --mount=type=cache,id=pyparser-docling-models,target=/var/cache/docling \
    python -c "from huggingface_hub import snapshot_download; snapshot_download(repo_id='docling-project/docling-layout-heron', revision='8f39ad3c0b4c58e9c2d2c84a38465abf757272d8', cache_dir='/var/cache/docling', local_dir='/opt/docling-models/layout-heron-8f39ad3c0b4c58e9c2d2c84a38465abf757272d8')" \
    && chmod -R a+rX "$DOCLING_ARTIFACTS_PATH" \
    && python -m pip uninstall -y pip

ARG PUBLIC_PARSER_DEPENDENCY_KEY
LABEL io.llunde.parser.dependencies.key="${PUBLIC_PARSER_DEPENDENCY_KEY}"
