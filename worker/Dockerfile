# Build context must be the agent-fleet/ repo root (not worker/):
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

# rtk (Rust Token Killer) — same tool-output compression proxy Mohammad runs
# locally (github.com/rtk-ai/rtk, Homebrew formula "rtk"). Static musl
# binary from GitHub releases, no Rust toolchain needed in this image.
RUN curl -fsSL https://github.com/rtk-ai/rtk/releases/latest/download/rtk-x86_64-unknown-linux-musl.tar.gz \
    | tar -xz -C /usr/local/bin rtk

# ponytail plugin (lean-code persona, matches Mohammad's own local Claude
# Code setup — github.com/DietrichGebert/ponytail). Marketplace add/install
# are git-clone + file-copy operations, no Anthropic auth needed at build
# time.
RUN claude plugin marketplace add DietrichGebert/ponytail \
    && claude plugin install ponytail@ponytail

WORKDIR /app
COPY worker ./worker
RUN cd worker && bun install --production

WORKDIR /app/worker
CMD ["bun", "run", "src/index.ts"]
