FROM docker.io/library/node@sha256:4d676821dff059fd00d277ee4261ef34ea712317fed0737c03941481b5760c96 AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
ENV VITE_BACKEND_URL=/api
RUN npm run build

FROM docker.io/library/caddy@sha256:13ba145cba2f3e28fa801994876e4c086d1b95d5aa2a520a734765ffb6b12017
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/fredrir/Y"
LABEL org.opencontainers.image.revision=$REVISION
RUN setcap -r /usr/bin/caddy
COPY --from=build /app/dist /srv
COPY --chmod=644 containers/Caddyfile /etc/caddy/Caddyfile
USER 1000:1000
EXPOSE 8080
