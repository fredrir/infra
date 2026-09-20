FROM docker.io/library/node@sha256:729cbbdccbbac8f9354c9ddaed8cfa6fe5ec893dca12ea52e9c13b4b76f7282b AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
ENV VITE_BACKEND_URL=/api
RUN npm run build

FROM ghcr.io/fredrir/platform-caddy@sha256:b03ceb80193dea33aa4812a190a08465dbe56f6a6aec364a47c15df1f56c8106
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/fredrir/Y"
LABEL org.opencontainers.image.revision=$REVISION
RUN apk upgrade --no-cache
COPY --from=build /app/dist /srv
COPY --chmod=644 containers/Caddyfile /etc/caddy/Caddyfile
USER 1000:1000
EXPOSE 8080
