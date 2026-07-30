# Build context is agent-fleet/ repo root:
#   docker build -f bot/Dockerfile -t <tag> .
FROM oven/bun:1-slim
RUN apt-get update && apt-get install -y --no-install-recommends postgresql-client ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY bot ./bot
COPY db ./db
RUN cd bot && bun install --production

WORKDIR /app/bot
CMD ["bun", "run", "src/index.ts"]
