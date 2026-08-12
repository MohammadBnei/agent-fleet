#!/usr/bin/env bash
set -euo pipefail

# Env contract (set by e2e-provisioner's Pod spec):
#   E2E_WORKTREE_PATH     - path to the task's git worktree (subPath-mounted)
#   E2E_START_CMD         - shell command that builds/runs the target app
#   E2E_APP_PORT          - port the app should listen on
#   E2E_CODE_SERVER_PORT  - port code-server listens on
#   E2E_PLAYWRIGHT_PORT   - port the Playwright MCP server listens on
#   E2E_EXEC_PORT         - port the run_command MCP listener (execmcp) listens on

: "${E2E_WORKTREE_PATH:?E2E_WORKTREE_PATH is required}"
: "${E2E_START_CMD:?E2E_START_CMD is required}"
: "${E2E_APP_PORT:?E2E_APP_PORT is required}"
: "${E2E_CODE_SERVER_PORT:?E2E_CODE_SERVER_PORT is required}"
: "${E2E_PLAYWRIGHT_PORT:?E2E_PLAYWRIGHT_PORT is required}"
: "${E2E_EXEC_PORT:?E2E_EXEC_PORT is required}"
: "${E2E_SSH_PORT:?E2E_SSH_PORT is required}"

# ponytail: SSH host key per-pod runtime keygen (ed25519 only, ~30ms).
# Image ships no host keys (Dockerfile rm after openssh-server install).
# Pods ephemeral, access via kubectl port-forward, StrictHostKeyChecking=no correct-by-design.
mkdir -p /etc/ssh /root/.ssh
ssh-keygen -q -t ed25519 -N "" -f /etc/ssh/ssh_host_ed25519_key
if [ -f /ssh-authorized-keys/authorized_keys ]; then
	cp /ssh-authorized-keys/authorized_keys /root/.ssh/
	chmod 600 /root/.ssh/authorized_keys
fi
/usr/sbin/sshd -e -p "${E2E_SSH_PORT}"

# Auth is enforced one layer up (Traefik + the existing basic-admin-auth
# Middleware) — code-server's own auth would just double-prompt.
code-server --bind-addr "0.0.0.0:${E2E_CODE_SERVER_PORT}" --auth none "${E2E_WORKTREE_PATH}" &

(cd "${E2E_WORKTREE_PATH}" && PORT="${E2E_APP_PORT}" bash -lc "${E2E_START_CMD}") &

# ponytail: --port switches @playwright/mcp from stdio to HTTP transport —
# verify this exact flag against the installed version (see docs/adr/0012
# risks; not verifiable from this repo alone before a real image build).
# --host 0.0.0.0 fixes ADR-0039's noted bug: default ::1:8931 (IPv6 localhost)
# made Service port routing fail, leaving Playwright tools unreachable.
bunx @playwright/mcp --host 0.0.0.0 --port "${E2E_PLAYWRIGHT_PORT}" --headless &

execmcp --port "${E2E_EXEC_PORT}" &

wait -n
