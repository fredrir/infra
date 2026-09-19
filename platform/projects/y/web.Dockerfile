FROM docker.io/library/node@sha256:cad3d421752df04025739eab1cec176426916faec228ff684211394c5bc8e339 AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
ENV VITE_BACKEND_URL=/api
RUN npm run build

FROM ghcr.io/fredrir/platform-caddy@sha256:b6c38126fe81ea62bba23a7d2aaff7301119eb530a2baae957d7517680889bd8
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/fredrir/Y"
LABEL org.opencontainers.image.revision=$REVISION
RUN apk upgrade --no-cache
COPY --from=build /app/dist /srv
COPY --chmod=644 containers/Caddyfile /etc/caddy/Caddyfile
USER 1000:1000
EXPOSE 8080
