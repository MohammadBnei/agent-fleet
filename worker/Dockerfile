# Build context must be the agent-fleet/ repo root (not worker/), since this
# also needs mcp-redis/ as a sibling directory:
#   docker build -f worker/Dockerfile -t <tag> .
FROM node:22-slim AS build
WORKDIR /build
COPY mcp-redis ./mcp-redis
COPY worker ./worker
RUN cd mcp-redis && npm install && npm run build
RUN cd worker && npm install && npm run build

FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code

WORKDIR /app
COPY --from=build /build/mcp-redis/dist ./mcp-redis/dist
COPY --from=build /build/mcp-redis/node_modules ./mcp-redis/node_modules
COPY --from=build /build/worker/dist ./worker/dist
COPY --from=build /build/worker/node_modules ./worker/node_modules

ENV MCP_REDIS_ENTRY=/app/mcp-redis/dist/index.js
WORKDIR /app/worker
CMD ["node", "dist/index.js"]
