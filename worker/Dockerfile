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
# Pinned, like the SDK in worker/package.json — and for the same reason that
# comment gives. This install was unpinned, so it re-resolved on every rebuild
# while the SDK beside it was held exact, which defeated the pin: the image
# carries TWO Claude Code versions (this global CLI, and the one bundled inside
# @anthropic-ai/claude-agent-sdk), and only one of them was controlled.
#
# The global CLI is what the `claude plugin install` steps below run at BUILD
# time. The SDK's bundled copy is what actually runs a session. They are
# allowed to differ; they are not allowed to drift silently.
ARG CLAUDE_CODE_VERSION=2.1.233
RUN bun install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

# The toolchain the session's builds and tests actually run on. This used to
# live in a second pod that the fleet staged from a per-repo "recipe"; after
# docs/adr/0048 §6 the agent builds and tests in its own pod with plain Bash,
# so the toolchain has to be here.
#
# Go is installed from upstream rather than apt: Debian slim's golang lags far
# enough behind to fail on a modern go.mod's toolchain directive.
ARG GO_VERSION=1.26.6
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local
ENV PATH="/usr/local/go/bin:${PATH}"

# Playwright's MCP server and system libraries — but NOT the browser binaries.
#
# `playwright install --with-deps chromium` used to run here and cost 2.45 GB
# of the image: 2.0 GB of browser builds, ~400 MB of X/graphics system libs,
# ~40 MB of npm packages. Only the last two actually need to be baked. The
# browsers are large, version-pinned, and never written at runtime, so they
# live once on the shared PVC and are mounted read-only at
# PLAYWRIGHT_BROWSERS_PATH (provisioner/internal/k8s/pod.go) instead of being
# copied into a layer on every node for every image version.
#
# `install-deps` rather than `install --with-deps`: same apt packages, no
# browser download. The deps must stay in the image regardless — they are apt
# packages installed as root at build time, which a mounted directory cannot
# provide.
#
# Verified by driving a real browser_navigate against a read-only mount, not
# by checking that a file exists — docs/adr/0044's whole lesson.
#
# Both packages are PINNED, and that matters more now than it used to. The
# browser builds live on the shared PVC (docs/adr/0051) and are installed by a
# Job running this image, so the build numbers these two versions resolve are
# what the cache holds. Unpinned, any rebuild could shift them and leave every
# session pointed at a browser build that is no longer there. EnsureBrowserCache
# recreates its Job when the image changes, so a deliberate bump repairs the
# cache on the next provisioner start — but only because the bump is deliberate.
#
# docs/adr/0044's finding is intact and still load-bearing:
# @playwright/mcp bundles a DIFFERENT playwright-core than `playwright` does,
# so it resolves a different browser build number and refuses to start on the
# one the first install put down. BOTH browser builds are required, and the
# server must be launched with `--browser chromium` (see session.ts). That is
# now the browser-cache Job's job (provisioner/internal/k8s/browsercache.go),
# which runs both install commands against the shared PVC — the two halves
# still have to stay together, they just live somewhere else.
ARG PLAYWRIGHT_VERSION=1.62.1
ARG PLAYWRIGHT_MCP_VERSION=0.0.79
RUN bun install -g playwright@${PLAYWRIGHT_VERSION} @playwright/mcp@${PLAYWRIGHT_MCP_VERSION} \
    && bunx playwright install-deps chromium \
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
    # Same treatment for `playwright` itself, and for a second consumer: the
    # browser-cache Job (provisioner/internal/k8s/browsercache.go) runs BOTH
    # installers, and it used to reach them via `bunx`. `bunx @playwright/mcp`
    # re-resolved the package from npm at Job runtime, which pinning here cannot
    # reach — so the cache held whatever build *latest* wanted while every
    # session ran the version pinned above. Naming a binary on PATH is what
    # makes these ARGs the single source of the build numbers.
    && ln -sf /root/.bun/install/global/node_modules/playwright/cli.js /usr/local/bin/playwright \
    && chmod +x /usr/local/bin/playwright \
    # RUN, not `test -x` — a symlink to a file that cannot execute is still a
    # symlink, and it fails in the Job rather than here. docs/adr/0044.
    && playwright --version \
    && playwright-mcp --version

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
#
# Since SDK 0.3.x the CLI is no longer a `cli.js` inside the SDK package: it is
# a NATIVE binary shipped as a per-platform optional dependency. Two resolve on
# linux-x64 (glibc + musl, ~300 MB each) and only the glibc one can run on this
# Debian base, so the musl copy is 300 MB of layer that can never execute.
#
# The binary is RUN, not `test -x`'d: running it is what proves it can run —
# the musl build is executable too, and fails only when something exec's it,
# i.e. in every session rather than once here. Its `--version` also gives the
# only cheap read of the SDK's bundled Claude Code, which is the version that
# actually serves a session (the global CLI above is build-time only). Asserting
# the two match is what makes the comment on CLAUDE_CODE_VERSION structural:
# they are allowed to differ, they are not allowed to drift silently. A bump to
# either one that leaves the other behind now fails the build with both numbers
# in the message.
#
# linux-x64 is hardcoded because PLATFORMS in .github/workflows/docker.yml is
# linux/amd64. Adding an arch there breaks this line — deliberately: the
# alternative is an image whose agent binary is for the wrong architecture.
RUN set -eu; \
    cd worker; \
    bun install --production --frozen-lockfile; \
    sdk_cli="$(node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude --version | cut -d' ' -f1)"; \
    if [ "$sdk_cli" != "${CLAUDE_CODE_VERSION}" ]; then \
        echo "SDK-bundled Claude Code is $sdk_cli but the global CLI is pinned to ${CLAUDE_CODE_VERSION}; bump one to match the other"; \
        exit 1; \
    fi; \
    rm -rf node_modules/@anthropic-ai/claude-agent-sdk-linux-x64-musl; \
    chmod +x entrypoint.sh; \
    chown -R bun:bun /app

WORKDIR /app/worker
USER bun
CMD ["./entrypoint.sh"]
