# Build context must be the agent-fleet/ repo root (not thot/):
#   docker build -f thot/Dockerfile -t <tag> .
#
# Bun runs the TypeScript sources directly, same as worker/'s image — no
# separate tsc build stage/dist output needed.
FROM oven/bun:1-slim

# Unlike worker/, thot never runs git/gh itself in this phase (Phase 8
# adds that later, at which point this image gains the same git/gh/claude
# CLI installs worker/Dockerfile already has) — kept minimal for now
# rather than front-loading tooling nothing yet uses.
RUN bun install -g @anthropic-ai/claude-code

WORKDIR /app
COPY thot ./thot
RUN cd thot && bun install --production

WORKDIR /app/thot
USER bun
CMD ["bun", "run", "src/index.ts"]
