#!/usr/bin/env bash
# NOTE: -e is deliberately absent (see the supervise() comment below).
set -uo pipefail

# This pod is a SANDBOX that MAY ALSO run an app (docs/adr/0044).
#
# Since docs/adr/0039 its primary job is being the worker's build/test
# sandbox — execmcp, i.e. `run_command`. Serving the app preview is the
# secondary job. Nothing inside this container decides to end the pod: PID 1
# outlives every child, and the pod's lifetime is owned exclusively by
# whoever deletes it (kill_env, core's teardowns, the reconcile sweep).
#
# This file used to end in `wait -n`, which returns as soon as the FIRST
# background job exits — so a failing `bun install`, a one-shot start command
# or a crashed dev server killed PID 1, and with RestartPolicy: Never the
# whole pod went Failed. That defeated the stay-alive guarantee ADR-0036
# thought the readiness probe was providing (it only ever held for an app
# that HANGS, not one that EXITS).
#
# Env contract (set by the provisioner's Pod spec):
#   E2E_WORKTREE_PATH     - path to the task's git worktree (subPath-mounted)
#   E2E_START_CMD         - OPTIONAL. Shell command that builds/runs the target
#                           app. Empty means a sandbox-only pod: no app, no
#                           preview, run_command still fully works. That is the
#                           correct state for a repo with no e2e profile.
#   E2E_APP_PORT          - port the app should listen on
#   E2E_CODE_SERVER_PORT  - port code-server listens on
#   E2E_PLAYWRIGHT_PORT   - port the Playwright MCP server listens on
#   E2E_EXEC_PORT         - port the run_command MCP listener (execmcp) listens on
#   E2E_SSH_PORT          - port sshd listens on

: "${E2E_WORKTREE_PATH:?E2E_WORKTREE_PATH is required}"
: "${E2E_APP_PORT:?E2E_APP_PORT is required}"
: "${E2E_CODE_SERVER_PORT:?E2E_CODE_SERVER_PORT is required}"
: "${E2E_PLAYWRIGHT_PORT:?E2E_PLAYWRIGHT_PORT is required}"
: "${E2E_EXEC_PORT:?E2E_EXEC_PORT is required}"
: "${E2E_SSH_PORT:?E2E_SSH_PORT is required}"
E2E_START_CMD="${E2E_START_CMD:-}"

# Fixed, documented path — fleet-shared/CLAUDE.md tells the agent to tail it
# and .claude/skills/fleet-debug references it. Deliberately NOT under
# $E2E_WORKTREE_PATH: that is the task's git worktree, shared with the worker
# pod, so a log file there would land in `git status` and get committed.
APP_LOG=/tmp/e2e-app.log

# supervise restarts a long-lived server forever. The three servers are all
# idempotent, instant to start and side-effect-free, so restarting them is
# always the right answer — unlike the app command (see below), whose failures
# are deterministic and expensive to retry.
#
# This is why `set -e` is off for the whole file: with -e, the first failing
# command inside one of these background subshells would exit the subshell and
# silently stop supervising, which is the current bug wearing a new hat.
supervise() {
	local label=$1
	shift
	while :; do
		"$@"
		echo "e2e-runner: ${label} exited ($?), restarting in 5s" >&2
		sleep 5
	done
}

# ponytail: SSH host key per-pod runtime keygen (ed25519 only, ~30ms).
# Image ships no host keys (Dockerfile rm after openssh-server install).
# Pods ephemeral, access via kubectl port-forward, StrictHostKeyChecking=no correct-by-design.
mkdir -p /etc/ssh /root/.ssh
ssh-keygen -q -t ed25519 -N "" -f /etc/ssh/ssh_host_ed25519_key
if [ -f /ssh-authorized-keys/authorized_keys ]; then
	cp /ssh-authorized-keys/authorized_keys /root/.ssh/
	chmod 600 /root/.ssh/authorized_keys
fi
# ponytail: sshd daemonizes (no -D), so it is not a job and is not supervised.
# It is a break-glass convenience, not a fleet dependency — if it dies, use
# code-server. Add -D + supervise if that ever proves wrong.
/usr/sbin/sshd -e -p "${E2E_SSH_PORT}"

# Auth is enforced one layer up (Traefik + the existing basic-admin-auth
# Middleware) — code-server's own auth would just double-prompt.
supervise code-server \
	code-server --bind-addr "0.0.0.0:${E2E_CODE_SERVER_PORT}" --auth none "${E2E_WORKTREE_PATH}" &

# --port switches @playwright/mcp from stdio to HTTP transport. VERIFIED
# against a real image build (docs/adr/0044): :8931 comes up bound, closing
# the open question ADR-0012/0036/0039 all carried as "not verifiable from
# this repo alone".
# --host 0.0.0.0 fixes ADR-0039's noted bug: default ::1:8931 (IPv6 localhost)
# made Service port routing fail, leaving Playwright tools unreachable.
#
# --allowed-hosts '*' is what actually makes it reachable, and binding was
# never the whole story. @playwright/mcp defaults this to "the host the
# server is bound to" and answers every other Host header with
# `403 Access is only allowed at localhost:8931`. The provisioner dials
# http://e2e-<id>.agent-fleet.svc.cluster.local:8931/mcp, so it got a 403 on
# every single call — and ProxiedTools swallows that into a nil tool list at
# log level Info, so browser automation was silently absent, not broken-loudly.
#
# '*' disables a DNS-rebinding guard whose threat model is a browser on a
# user's machine reaching a localhost server. That cannot happen here:
# k8s/provisioner/networkpolicy.yaml admits :8931 from the provisioner pod
# ONLY (Traefik gets app+code-server, other fleet pods get :3000), and the
# port has no IngressRoute. Cilium enforcing L3 is a strictly stronger
# boundary than a Host header string match. If :8931 ever gains an
# IngressRoute, narrow this to the pod's own service DNS name instead.
supervise playwright-mcp \
	bunx @playwright/mcp --host 0.0.0.0 --port "${E2E_PLAYWRIGHT_PORT}" --headless --allowed-hosts '*' &

supervise execmcp execmcp --port "${E2E_EXEC_PORT}" &

# ponytail: the app runs ONCE and is never restarted. Its failures are
# deterministic — a failing `bun install`, a command binding 127.0.0.1, a
# stale profile — so a restart loop would re-run a 782s install (ADR-0036)
# forever AND hide the failure: a crashlooping app looks identical to a slow
# one on the dashboard card. The agent just edited this code and has
# run_command; restarting is its deliberate call. Supervise with backoff only
# if "the app died and nobody noticed" shows up in practice.
if [ -n "${E2E_START_CMD}" ]; then
	# tee, not a plain redirect: the same output has to reach both the file the
	# agent tails and this container's stdout, which is what Loki indexes
	# (view_logs component=e2e). PIPESTATUS[0], not $?, because $? is tee's.
	(
		cd "${E2E_WORKTREE_PATH}" || exit
		PORT="${E2E_APP_PORT}" bash -lc "${E2E_START_CMD}" 2>&1 | tee "${APP_LOG}"
		# Captured immediately: the next pipeline overwrites PIPESTATUS.
		app_status=${PIPESTATUS[0]}
		echo "--- e2e app command exited with status ${app_status} ---" | tee -a "${APP_LOG}"
		echo "--- the sandbox stays up; fix the cause, then restart it with run_command ---" | tee -a "${APP_LOG}"
	) &
else
	echo "e2e-runner: no E2E_START_CMD — sandbox-only pod (no app, no preview). run_command is unaffected." | tee "${APP_LOG}"
fi

# Bare `wait`: blocks on every background job. The supervise loops never
# return, so this never returns either — PID 1 outliving the app is the whole
# point of docs/adr/0044.
wait
