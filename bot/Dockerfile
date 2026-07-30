# Build context is agent-fleet/ repo root:
#   docker build -f bot/Dockerfile -t <tag> .
FROM node:22-slim AS build
WORKDIR /build
COPY bot ./bot
RUN cd bot && npm install && npm run build

FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /build/bot/dist ./dist
COPY --from=build /build/bot/node_modules ./node_modules
COPY db ./db

CMD ["node", "dist/index.js"]
