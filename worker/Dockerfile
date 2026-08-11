# Build context must be the agent-fleet/ repo root (not worker/):
#   docker build -f worker/Dockerfile -t <tag> .
#
# Bun runs the TypeScript sources directly (no separate tsc build stage/dist
# output needed) — smaller image, faster startup, per Mohammad's call to
# standardize this repo's JS runtime on Bun.
FROM oven/bun:1-slim
# procps is not optional: the bundled Claude Code CLI kills subprocess trees
# via tree-kill, which on Linux shells out to `ps -o pid --no-headers --ppid
# <pid>`. oven/bun:1-slim ships no `ps`, and Bun's child_process.spawn throws
# ENOENT *synchronously* (Node emits an async 'error' event tree-kill handles),
# so the CLI died mid-session with "Claude Code process exited with code 1" —
# task 41b11fd4, 2026-08-11, hit while requesting an e2e env.
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates procps \
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

# ponytail + caveman plugins (lean-code / terse-output personas, match
# Mohammad's own local Claude Code setup — github.com/DietrichGebert/ponytail,
# github.com/JuliusBrussee/caveman). Marketplace add/install are git-clone +
# file-copy operations, no Anthropic auth needed at build time. Installed
# here into the image's default $HOME (/root/.claude); worker/entrypoint.sh
# copies that once into $CLAUDE_CONFIG_DIR at pod startup, since that's the
# PVC path Claude Code actually reads at runtime (docs/adr/0033). Perms
# opened up (not just this dir — see the `worker` user creation below) so
# the non-root `worker` user can still read it to do that copy.
RUN claude plugin marketplace add DietrichGebert/ponytail \
    && claude plugin install ponytail@ponytail
RUN claude plugin marketplace add JuliusBrussee/caveman \
    && claude plugin install caveman@caveman

# Claude Code refuses to run with permissionMode "bypassPermissions"
# (`--dangerously-skip-permissions`) as root/sudo — a real crash hit live
# (task 405e2cd0, 2026-08-10) once a task's mode got set to
# bypassPermissions via the dashboard and every resume inherited it. `git`/
# `gh` push still work non-root; auth is a token (GH_TOKEN), not an
# OS-level credential. Reuses the base image's own `bun` user (uid 1000)
# rather than creating a new one. chmod opens /root so entrypoint.sh's
# plugin copy (above) still works post-USER-switch.
RUN chmod -R o+rX /root

WORKDIR /app
COPY worker ./worker
# --frozen-lockfile: the SDK bundles the whole Claude Code CLI, so a version
# drift here swaps the agent's entire runtime — including which binaries it
# shells out to. `^0.1.0` let that happen silently on any rebuild; the version
# is now exact in package.json and the build fails loudly if the lockfile
# disagrees rather than resolving something new.
RUN cd worker && bun install --production --frozen-lockfile && chmod +x entrypoint.sh && chown -R bun:bun /app

WORKDIR /app/worker
USER bun
CMD ["./entrypoint.sh"]
