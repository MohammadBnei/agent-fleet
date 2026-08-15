# ADR-0036: The e2e recipe is readable by the agent, its override is human-approved, and the app port is probed

**Status:** Superseded by [0048](0048-one-session-one-pod-one-shared-home.md) — with the recipe system deleted there is no resolved recipe to make visible and no override to approve
**Date:** 2026-08-11

## Context

`docs/adr/0034` built the environment recipe system — dashboard-editable
`repo_profiles`, a bounded ingredient catalog, provisioner-minted
credentials — and its seeded `dream-analyst` e2e profile is correct
(`cd front && bun install && bunx prisma migrate deploy && bunx vite dev
--host 0.0.0.0 --port $PORT`). The preview still returned Bad Gateway.

Live debugging on `ukubi-cluster` found **three independent faults**, none
of which ADR-0034 covers:

1. **The e2e `Service` selector matched the worker pod too.**
   `provisioner/internal/k8s/service.go` selected on `TaskIDLabel` alone,
   and `WorkerLabels` carries that same label. The EndpointSlice held both
   `e2e-07c17c6605994bffab6c` and `worker-07c17c6605994bffab6c-nd8zd`, so
   roughly half of Traefik's requests were load-balanced onto a pod with
   nothing listening on `:3000`. This alone breaks the preview even with a
   perfect start command.

2. **The agent overrode a profile it had no way to read.** The pod ran
   `cd front && bun install && bun run dev` — verbatim the example string
   in `request_e2e_env`'s own MCP tool description. Vite ignored `$PORT`
   and bound `127.0.0.1:5173` (confirmed in the pod's `/proc/net/tcp6`).
   The `start_cmd` override existed, was silent, was never persisted, and
   nothing ever showed the human that it had displaced the profile.

3. **Nothing detected an app that never binds.** ADR-0034 claims "nothing
   in this design allows a pod to report running while silently broken" —
   true for tools and services (StartupProbe / pre-create minting), false
   for `start_cmd`, which had no probe at all. The pod sat `1/1 Running`
   for the entire failure.

An architecture interview (`architecture-interview` skill) established that
the config surface itself is not the problem — the dashboard's
`ManageRepoProfilesModal` works. The problem is that *the worker cannot see
it*, so it guesses, and its guess wins silently.

## Decision

### The resolved recipe is echoed back to the agent

`RequestE2eEnvResponse` gains `resolved_start_cmd`, `profile_name`,
`tools`, `services`, populated by `core` from the profile it already
resolves. The sidecar's `request_e2e_env` returns them in its tool result.
An agent that can read the recipe reports "the configured command binds
localhost" instead of inventing a replacement for it.

### A `start_cmd` override requires a human yes, and stays ephemeral

The `startCmd` parameter survives, but `sidecar/internal/mcpserver`'s
handler now blocks on the existing `AskUserQuestion` dashboard path
(`docs/adr/0018`) before forwarding a non-empty one. Anything other than an
explicit pick of the override — decline, timeout, malformed answer, RPC
failure — falls back to the profile, matching `docs/adr/0029`'s rule that a
decision is never inferred from silence.

Enforced in the handler, not left to the agent to ask: an agent choosing
whether to check is precisely how this failed.

An approved override applies to that task's pod only. `repo_profiles` is
never written by an agent — `docs/adr/0034` deferred agent-vs-human
authorization over recipes and that deferral stands.

### A readiness probe on the app port

The `e2e-runner` container gets a TCP `ReadinessProbe` on `AppPort`, so the
pod joins the Service's endpoints only once something actually listens on
`0.0.0.0:3000`.

Readiness specifically, **not** startup or liveness: a pod whose app never
binds must stay alive so code-server (`:8080`) is still reachable to debug
why. `FailureThreshold` is 120 at a 10s period (~20 min) because a cold
`bun install` measured **782 seconds** live — a tight window would report a
false failure on every cold cache.

### The dashboard shows what the agent sees

`GetE2eSessionStatus` (provisioner — the fleet's sole cluster-RBAC holder)
gains `start_cmd` read back off the live pod's own `E2E_START_CMD` env,
plus `pod_phase`, `app_ready`, `restarts`, `started_at`.
`DashboardService.GetE2eStatus` passes those through and adds the declared
recipe from `repo_profiles` plus `start_cmd_overridden`. A new `E2eCard` in
the task sidebar renders it, polled every 5s.

`app_ready` is the field that answers the original question: a card reading
**Running · not ready · `cd front && bun install && bun run dev`** says in
one glance both that it is broken and why.

## Alternatives considered

- **A YAML spec file in the target repo's git tree** — rejected, same
  reason `docs/adr/0034` rejected it: it lets a task/agent branch influence
  what containers the provisioner creates on its own behalf, reopening the
  git-write/infra-mutation separation `docs/adr/0012` exists to prevent.
- **A YAML file in `agent-fleet` as the profile source of truth** —
  rejected: duplicates a dashboard surface that already works, and
  reintroduces the redeploy-to-onboard-a-repo gap `docs/adr/0028` closed.
- **Let the agent write `repo_profiles` directly (self-healing)** —
  rejected: persists a guess to every future task on that repo, which is
  the original failure with a longer blast radius.
- **Agent proposes, human approves, and the approval persists to the
  profile** — rejected for now: a branch-specific command would leak into
  unrelated tasks. Ephemeral per-task approval keeps the profile a
  deliberate human edit. Promotion to the profile is a possible follow-up.
- **Remove the `startCmd` override entirely** — rejected: the agent has the
  worktree in front of it and sometimes genuinely does know better; the
  problem was that its knowing was unverified and invisible, not that it
  existed.
- **Block `request_e2e_env` until the app serves, then error with logs** —
  rejected: the 782s cold install means holding a gRPC call open for 13+
  minutes, and the timeout would be a guess.
- **A new pollable e2e-status RPC for the agent** — rejected: the agent
  already has Playwright MCP against the preview URL and can see the
  failure itself; with `resolved_start_cmd` in hand it can now explain it.
- **A startup or liveness probe instead of readiness** — rejected: both
  kill the pod on failure, taking code-server with them and removing the
  only way to debug an app that won't start.
- **Server-push (SSE/stream) for the e2e card** — rejected: the card's
  interesting transitions happen on a minutes-long timescale; a 5s poll of
  one small RPC is enough.

## Consequences

- `provisioner/internal/k8s.GetPod` returns a `PodState` struct rather than
  a bare phase; its four call sites read `.Phase`.
- `core` gains a second reason to read `repo_profiles` on the dashboard
  path (`GetE2eStatus`), best-effort — a missing task or profile still
  leaves the live pod state worth rendering.
- The e2e pod can now be `Running` and **not ready**, which is a new state
  for anything that reasons about e2e pods. `e2eStatusFromPhase` is
  unchanged: `status` still derives from phase alone, and `app_ready` is
  reported alongside it rather than folded into it.
- An agent's `startCmd` override now blocks on a human for up to 5 minutes.
  Agents that pass one reflexively will feel this; that is the intent.
- Not fixed here, same class of bug: Playwright MCP binds `::1:8931` in the
  e2e pod while `execmcp` correctly binds `[::]:8932`, so the `:8931`
  Service port routes to nothing (`e2e-runner/entrypoint.sh`).
