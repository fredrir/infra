FROM docker.io/library/node@sha256:4d676821dff059fd00d277ee4261ef34ea712317fed0737c03941481b5760c96 AS build
WORKDIR /app
COPY backend/package.json backend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY backend/ ./
RUN npx tsc -p . && cp -R src/schema dist/schema && npm prune --omit=dev --ignore-scripts && npm cache clean --force

FROM docker.io/library/node@sha256:4d676821dff059fd00d277ee4261ef34ea712317fed0737c03941481b5760c96
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/fredrir/Y"
LABEL org.opencontainers.image.revision=$REVISION
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /app/package.json ./
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
RUN mkdir -p /var/www/html/uploads && chown node:node /var/www/html/uploads && ln -s /var/www/html/uploads /app/dist/uploads
USER node
EXPOSE 3001
CMD ["node", "dist/index.js"]
