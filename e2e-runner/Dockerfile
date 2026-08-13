# Build context must be the agent-fleet/ repo root (not e2e-runner/), since
# execmcp needs go.work + proto/gen/go as siblings, same reason
# provisioner/Dockerfile's build context moved there:
#   docker build -f e2e-runner/Dockerfile -t <tag> .
FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY e2e-runner ./e2e-runner
RUN go work init ./proto/gen/go ./e2e-runner
WORKDIR /app/e2e-runner
RUN CGO_ENABLED=0 go build -o /out/execmcp ./cmd/execmcp

# One generic image for every target repo — mirrors how worker/'s single
# image is parametrized by TARGET_REPO/TARGET_REPO_URL/WORKER_NAME rather
# than a Dockerfile per repo. Per-repo behavior comes entirely from env vars
# set on the Pod (see entrypoint.sh).
#
# CPU-only headless Chromium for v1 — GPU acceleration is a deferred
# fast-follow (see docs/adr/0012), not built into this image.
FROM oven/bun:1-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git openssh-server \
    && rm -f /etc/ssh/ssh_host_* \
    && rm -rf /var/lib/apt/lists/*

# code-server — the human-review-only IDE surface.
RUN curl -fsSL https://code-server.dev/install.sh | sh

# Playwright + its own MCP server. `--with-deps` pulls the apt packages
# Chromium needs on Debian; no GPU driver packages, CPU-only rendering.
RUN bun install -g playwright @playwright/mcp \
    && bunx playwright install --with-deps chromium

COPY --from=build /out/execmcp /usr/local/bin/execmcp

# rtk (Rust Token Killer) — same binary worker/Dockerfile installs. Every
# build/test/install the agent runs lands here via run_command
# (docs/adr/0039), which is where the fleet's token spend actually is; the
# worker rewrites those commands to `rtk <cmd>` (worker/src/rtkHook.ts), so
# the binary has to exist in *this* image, not just the worker's.
#
# Pinned, not `releases/latest`: the tarball is the whole tool, and an
# unpinned URL re-rolls it on every rebuild — a bad upstream release would
# land in the fleet with no commit to point at. Bump both Dockerfiles
# together. Static musl binary, so no Rust toolchain and no runtime deps.
# amd64 only, matching docker.yml's PLATFORMS.
ARG RTK_VERSION=v0.45.0
RUN curl -fsSL https://github.com/rtk-ai/rtk/releases/download/${RTK_VERSION}/rtk-x86_64-unknown-linux-musl.tar.gz \
    | tar -xz -C /usr/local/bin rtk

WORKDIR /app
COPY e2e-runner/entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

# ponytail: no process supervisor (pm2/supervisord/s6) — entrypoint.sh's own
# supervise() is a 6-line while-loop over the three servers, and the app is
# deliberately NOT restarted (docs/adr/0044). Bring in a real supervisor only
# if something needs dependency ordering or per-process backoff policy.
ENTRYPOINT ["./entrypoint.sh"]
