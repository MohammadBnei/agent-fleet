# One generic image for every target repo — mirrors how worker/'s single
# image is parametrized by TARGET_REPO/TARGET_REPO_URL/WORKER_NAME rather
# than a Dockerfile per repo. Per-repo behavior comes entirely from env vars
# set on the Pod (see entrypoint.sh).
#
# CPU-only headless Chromium for v1 — GPU acceleration is a deferred
# fast-follow (see docs/adr/0012), not built into this image.
#
#   docker build -f e2e-runner/Dockerfile -t <tag> e2e-runner
FROM oven/bun:1-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# code-server — the human-review-only IDE surface.
RUN curl -fsSL https://code-server.dev/install.sh | sh

# Playwright + its own MCP server. `--with-deps` pulls the apt packages
# Chromium needs on Debian; no GPU driver packages, CPU-only rendering.
RUN bun install -g playwright @playwright/mcp \
    && bunx playwright install --with-deps chromium

WORKDIR /app
COPY entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

# ponytail: no process supervisor (pm2/supervisord) — the whole pod gets
# deleted on teardown, nothing inside it needs restart-on-crash.
ENTRYPOINT ["./entrypoint.sh"]
