# Build context must be the agent-fleet/ repo root (not worker/), since this
# also needs mcp-redis/ as a sibling directory:
#   docker build -f worker/Dockerfile -t <tag> .
#
# Bun runs the TypeScript sources directly (no separate tsc build stage/dist
# output needed) — smaller image, faster startup, per Mohammad's call to
# standardize this repo's JS runtime on Bun.
FROM oven/bun:1-slim
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
RUN bun install -g @anthropic-ai/claude-code

WORKDIR /app
COPY mcp-redis ./mcp-redis
COPY worker ./worker
RUN cd mcp-redis && bun install --production
RUN cd worker && bun install --production

ENV MCP_REDIS_ENTRY=/app/mcp-redis/src/index.ts
WORKDIR /app/worker
CMD ["bun", "run", "src/index.ts"]
