FROM docker.io/library/node@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS api-tests
WORKDIR /app
COPY backend/package.json backend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY backend/ ./
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project y --suite api

FROM docker.io/library/node@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS web-tests
WORKDIR /app
ENV CYPRESS_INSTALL_BINARY=0 VITE_BACKEND_URL=/api
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN --mount=type=bind,source=.infra-artifacts/infra,target=/usr/local/bin/infra \
    infra ci project check --project y --suite web
