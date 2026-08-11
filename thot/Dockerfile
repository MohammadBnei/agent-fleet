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

# kubectl is thot's actual cluster interface — it reaches the API server as
# its own in-cluster ServiceAccount, which kubectl auto-detects from the
# projected token mount. No kubeconfig, no credential ever leaves the
# cluster (the same mechanism ADR-0022's actions-runner uses).
#
# Every kubectl invocation still goes through canUseTool for a human
# decision, and the ClusterRole (infra-bootstrap's gitops/platform/thot/
# rbac.yaml) is the real enforcement boundary regardless of what gets
# typed — the image granting kubectl doesn't widen what it can do.
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/$(dpkg --print-architecture)/kubectl" -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY thot ./thot
RUN cd thot && bun install --production

WORKDIR /app/thot
USER bun
CMD ["bun", "run", "src/index.ts"]
