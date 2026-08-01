# Standalone image — unlike worker/, this doesn't need mcp-redis as a
# sibling, so the build context is just this directory:
#   docker build -f e2e-provisioner/Dockerfile -t <tag> e2e-provisioner
FROM oven/bun:1-slim
WORKDIR /app
COPY . .
RUN bun install --production
CMD ["bun", "run", "src/index.ts"]
