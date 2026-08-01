#!/usr/bin/env bash
set -euo pipefail

# Env contract (set by e2e-provisioner's Pod spec):
#   E2E_WORKTREE_PATH     - path to the task's git worktree (subPath-mounted)
#   E2E_START_CMD         - shell command that builds/runs the target app
#   E2E_APP_PORT          - port the app should listen on
#   E2E_CODE_SERVER_PORT  - port code-server listens on
#   E2E_PLAYWRIGHT_PORT   - port the Playwright MCP server listens on

: "${E2E_WORKTREE_PATH:?E2E_WORKTREE_PATH is required}"
: "${E2E_START_CMD:?E2E_START_CMD is required}"
: "${E2E_APP_PORT:?E2E_APP_PORT is required}"
: "${E2E_CODE_SERVER_PORT:?E2E_CODE_SERVER_PORT is required}"
: "${E2E_PLAYWRIGHT_PORT:?E2E_PLAYWRIGHT_PORT is required}"

# Auth is enforced one layer up (Traefik + the existing basic-admin-auth
# Middleware) — code-server's own auth would just double-prompt.
code-server --bind-addr "0.0.0.0:${E2E_CODE_SERVER_PORT}" --auth none "${E2E_WORKTREE_PATH}" &

(cd "${E2E_WORKTREE_PATH}" && PORT="${E2E_APP_PORT}" bash -lc "${E2E_START_CMD}") &

# ponytail: --port switches @playwright/mcp from stdio to HTTP transport —
# verify this exact flag against the installed version (see docs/adr/0012
# risks; not verifiable from this repo alone before a real image build).
bunx @playwright/mcp --port "${E2E_PLAYWRIGHT_PORT}" --headless &

wait -n
