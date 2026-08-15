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

# The toolchain the session's builds and tests actually run on. This used to
# live in a second pod that the fleet staged from a per-repo "recipe"; after
# docs/adr/0048 §6 the agent builds and tests in its own pod with plain Bash,
# so the toolchain has to be here.
#
# Go is installed from upstream rather than apt: Debian slim's golang lags far
# enough behind to fail on a modern go.mod's toolchain directive.
ARG GO_VERSION=1.26.5
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local
ENV PATH="/usr/local/go/bin:${PATH}"

# Playwright and its MCP server, carrying docs/adr/0044's finding intact:
# @playwright/mcp bundles a DIFFERENT playwright-core than `playwright` does,
# so it resolves a different browser build number and refuses to start on the
# one the first install put down. Both installs are required, and the server
# must be launched with `--browser chromium` (see session.ts).
#
# That was found only by actually driving a browser_navigate. Every cheaper
# check passed: the port was bound, the server answered, the handshake
# succeeded, the tool list registered — and the browser binary was absent.
# Browsers go to a fixed, world-readable path rather than $HOME/.cache.
#
# Without this they install under ROOT's cache at build time, and the session
# runs as `bun` — so Playwright looked in /home/bun/.cache/ms-playwright and
# reported the browser as not installed. Caught by actually driving a
# browser_navigate in the built image; `playwright-mcp --version` and the MCP
# handshake both passed with the browser absent, which is docs/adr/0044's
# lesson repeating itself in a new location.
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN bun install -g playwright @playwright/mcp \
    && bunx playwright install --with-deps chromium \
    && bunx @playwright/mcp install-browser chrome-for-testing \
    && chmod -R o+rX /ms-playwright \
    # bun's global install does NOT put @playwright/mcp's own bin on PATH —
    # only `claude` and `playwright-core` end up in the global .bin, so the
    # server was unreachable by name. Caught by running the binary in the built
    # image rather than by trusting the install to have worked, which is the
    # whole lesson of docs/adr/0044. Symlinked explicitly so session.ts can
    # name a command instead of hardcoding a node_modules path.
    #
    # -f, because whether bun links this bin itself depends on the
    # @playwright/mcp release: a version that does link it makes a bare `ln -s`
    # fail the build on "File exists". Overwriting is safe — same target.
    && ln -sf /root/.bun/install/global/node_modules/@playwright/mcp/cli.js /usr/local/bin/playwright-mcp \
    && chmod +x /usr/local/bin/playwright-mcp \
    # Braced, because `a && b || true` binds the || to the WHOLE chain: written
    # flat, a failed `playwright install` would still exit 0 and ship an image
    # with no browser — the exact docs/adr/0044 failure the block above exists
    # to prevent, reintroduced by shell precedence. Only this one chmod is
    # allowed to fail (the path may not exist), and only it.
    && { chmod -R o+rX /root/.cache 2>/dev/null || true; }

# rtk (Rust Token Killer) — same tool-output compression proxy Mohammad runs
# locally (github.com/rtk-ai/rtk, Homebrew formula "rtk"). Static musl
# binary from GitHub releases, no Rust toolchain needed in this image.
#
# Pinned, not `releases/latest`: the tarball is the whole tool, and an
# unpinned URL re-rolls it on every rebuild. Keep in step with
# e2e-runner/Dockerfile, which installs it for the same reason. The hook
# that actually uses it lives in worker/src/rtkHook.ts — a settings.json
# entry does nothing under the Agent SDK.
ARG RTK_VERSION=v0.45.0
RUN curl -fsSL https://github.com/rtk-ai/rtk/releases/download/${RTK_VERSION}/rtk-x86_64-unknown-linux-musl.tar.gz \
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
