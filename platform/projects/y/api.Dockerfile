FROM docker.io/library/node@sha256:fa271c47a5d81dc321f4a45be01362f5b3de7559edc7e76b8c4089be1e50d866 AS build
WORKDIR /app
COPY backend/package.json backend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY backend/ ./
RUN npx tsc -p . && cp -R src/schema dist/schema && npm prune --omit=dev --ignore-scripts && npm cache clean --force

FROM docker.io/library/node@sha256:fa271c47a5d81dc321f4a45be01362f5b3de7559edc7e76b8c4089be1e50d866
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/fredrir/Y"
LABEL org.opencontainers.image.revision=$REVISION
RUN sed -i 's|http://|https://|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /app/package.json ./
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
RUN mkdir -p /var/www/html/uploads && chown node:node /var/www/html/uploads && ln -s /var/www/html/uploads /app/dist/uploads
USER node
EXPOSE 3001
CMD ["node", "dist/index.js"]
