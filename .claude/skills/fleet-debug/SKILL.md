---
name: fleet-debug
description: Diagnose a stuck or failed agent-fleet task — task/journal state, worker/sidecar/provisioner pod logs, and known failure modes. Use when a Discord task thread has gone quiet, a task never opened a PR, or a worker looks stuck/crashed.
user-invocable: true
allowed-tools:
  - Read
  - Bash(psql *)
  - Bash(kubectl logs *)
  - Bash(kubectl get *)
  - Bash(kubectl describe *)
---

# /fleet-debug — diagnose a stuck or failed task

Work outward from the database, then the transcript, then live pod logs.
Full flow reference: `docs/ARCHITECTURE.md` §2. As of `docs/adr/0019`–
`0021` there's no per-repo Deployment and no Redis — a task's worker is a
single-shot, two-container Pod (`worker-<shortTaskId>`, see
`provisioner/internal/k8s/names.go`'s `WorkerResourceName`) spawned by the
provisioner, and the planning transcript is a durable Postgres table, not
a Redis list.

## 1. Check task state first

```sql
SELECT id, repo, status, claimed_by, heartbeat_at, lease_id, pr_url, updated_at
FROM tasks WHERE id = '<taskId>';
```

- `pending` and never claimed → `core`'s dispatch loop
  (`core/internal/dispatch/loop.go`) may be stalled, `MAX_IN_FLIGHT_TASKS`
  headroom may be exhausted (`CountInFlight`), or the repo isn't in
  `core/internal/tasks/store.go`'s `KnownRepos` map (dispatch logs
  `claimed task for unknown repo, cannot dispatch` and gives up silently
  from the task's perspective).
- `claimed`/`planning`/`implementing` and `heartbeat_at` is stale (>10min,
  no recent writes) → the worker pod likely crashed. `ClaimNextTask` will
  reclaim it on `core`'s next dispatch tick and spawn a *fresh* pod for
  the same task — check `knowledge_journal` for whether that already
  happened (a second `task.claimed`-adjacent event) before assuming
  nothing is happening.
- `failed` → the error is in the Discord thread's last message and in
  `knowledge_journal` (below).

## 2. Read the journal — the real signal, not just status

```sql
SELECT event_type, payload, created_at
FROM knowledge_journal
WHERE payload->>'taskId' = '<taskId>'
ORDER BY created_at;
```

This now also carries **provisioner pod-lifecycle events**
(`event_type` `pod.created`/`pod.scheduled`/`pod.running`/`pod.crashed`/
`pod.terminated`, pushed live over gRPC via `ReportPodEvents` — see
`docs/adr/0020` point 3) — check these first if the task never seems to
have gotten a pod at all: a `pod.crashed` event with `message` like
`clone/fetch failed: ...` or `worktree add failed: ...` means the
provisioner's own git step failed before a worker pod was ever created,
which won't show up in the worker's own logs since it never ran.

Look also at `session.result`-style events' turn/cost figures. **A
session that burned turns but posted nothing to the transcript** is the
signature of a missing `allowedTools` entry — an MCP tool call not on the
list is silently denied by the SDK, not errored loudly (`docs/adr/0008`).
Since `docs/adr/0017`, planning is one continuous session (not a
proposer/critic pair) that also runs implementation in the same session
(`docs/adr/0021`) — a `Task`-tool permission denial specifically means
`doubt-driven-development`'s fresh-context subagent spawn was silently
blocked.

## 3. Check the worker pod's live logs — two containers, not one

```
kubectl get pod -n agent-fleet -l agent-fleet.dev/task-id=<taskId>
kubectl logs -n agent-fleet worker-<shortTaskId> -c worker -f
kubectl logs -n agent-fleet worker-<shortTaskId> -c sidecar -f
```

(`<shortTaskId>` is the task UUID with dashes stripped, truncated to 20
chars — see `WorkerResourceName`/`shortID` in `provisioner/internal/k8s/
names.go`; easiest to just find it via the label selector above.)

Every SDK message is logged in the `worker` container, not just the final
result (`logSdkMessage` in `worker/src/planning.ts`) — `tool_use` and
`tool_result` entries (with `isError`) show up here in real time. The
`sidecar` container's logs show every gRPC call it proxied to `core` (and
its own independent telemetry push failures/successes) — check it
separately if the `worker` container looks like it's making tool calls
that never seem to have any effect (e.g. `send_message` calls that never
reach Discord): the break could be sidecar→core connectivity, not the
agent session itself.

If the pod doesn't exist at all: check §2's `pod.*` journal events first
— either the provisioner never got the `CreateWorkerPod` call (dispatch
loop issue, see §1) or it got it and the pod create itself failed
(`pod create failed: ...` in the journal payload).

## 4. Inspect the planning transcript directly

```sql
SELECT seq, "from", type, text, created_at
FROM planning_transcript
WHERE task_id = '<taskId>'
ORDER BY seq;
```

Each row is one `send_message`/human-reply/checkpoint entry — `type` is
`discussion`/`approve`/`abort`/`question`/`answer`/`tool_call` (the last
one is the sidecar's own telemetry, never relayed to Discord — see
`relay_dead_letter`/`relay_attempts` below). If Discord shows the thread
went quiet but this table is still growing, `core`'s own relay loop
(`core/internal/transcript/relay.go`'s `RelayLoop`/`relayPending`) is the
broken link, not the planning logic itself — check `relayed_to_discord`/
`relay_attempts`/`relay_dead_letter`/`relay_last_error` on the relevant
rows, and `core`'s own logs for Discord API errors.

## 5. The e2e sandbox (`run_command`, the preview)

Since `docs/adr/0039` the e2e pod is the agent's build/test sandbox, so
"`run_command` is failing" is a task-blocking bug, not a preview cosmetics
one. `docs/adr/0044` reshaped it — check in this order:

```bash
# Is there a pod at all, and what phase?
kubectl -n agent-fleet get pods -l app.kubernetes.io/component=e2e-run

# Why is it not Running? (ImagePullBackOff, OOMKilled, admission rejection)
kubectl -n agent-fleet describe pod e2e-<first 20 chars of task id>

# The app's own output — the first thing to read when the preview 502s.
# Ends with an explicit exit-status marker if the command died.
kubectl -n agent-fleet exec e2e-<id> -- tail -50 /tmp/e2e-app.log
```

- **The pod stays up even when the app is dead — that is by design**
  (`adr/0044`). A `Running` pod whose preview 502s is the *normal* shape of
  a broken start command, not a second bug. `run_command` and code-server
  both still work; use them.
- **`app_ready: false` with an empty `start_cmd` is not a failure.** It's a
  sandbox-only pod: the repo's profile has no app command, nothing binds the
  app port, and the readiness probe correctly never passes. The dashboard
  card says `sandbox · no app`.
- **The app is never restarted automatically.** A dead app stays dead for the
  rest of the session unless the agent restarts it via `run_command`.
- **`run_command` "not reachable" on the first call of a session** is usually
  just a cold pod — the sidecar retries with backoff for ~65s, and the final
  error names the pod's actual phase. `requested` means still pulling/
  installing; `failed` means call `kill_env` and let it rebuild.
- **A sandbox that vanished mid-session** was probably swept: the reconcile
  loop GCs terminal-phase pods and any pod older than `E2E_MAX_AGE_MS`
  (default 24h). It reports a `SESSION_KIND_E2E` event, so it's in the
  journal.
- **Which profile the sandbox was built from** comes from the repo's
  `e2e_profile` column, not a hardcoded `"e2e"`. A sandbox missing a
  toolchain almost always means that column points at the wrong profile (or
  the profile lacks the tool). `request_e2e_env`'s response echoes
  `profileName`/`tools`/`services` — read it before assuming a bug.

## Known failure modes

- **Missing `allowedTools` entry** → silent permission denial, burned
  turns, zero transcript output. Fix: add the tool to
  `worker/src/planning.ts`'s `allowedTools` list (see `/fleet-feature`).
- **Worker pod crash mid-task** → single-shot pods don't retry themselves
  (`docs/adr/0019`); the *provisioner*'s worktree cleanup only runs via
  `core`'s `TearDownSession` call, which a crashed pod never triggered —
  the worktree can be left behind on the shared PVC until
  `ClaimNextTask`'s heartbeat reclaim eventually lets a fresh pod reuse
  the same task ID's worktree path (worktree creation handles this: stale
  leftovers are removed before re-adding, see `provisioner/internal/git/
  git.go`'s `CreateWorktree`).
- **`total_cost_usd` looks wrong/zero** → expected under subscription auth
  (`CLAUDE_CODE_OAUTH_TOKEN`, not a metered API key) — it's a notional
  figure, not a real charge (`docs/adr/0004`). Not a bug by itself.
- **`Task` tool permission denial during planning** → `doubt-driven-
  development` never actually ran its fresh-context review despite the
  planner saying it would; the model has no visible error to react to, it
  just silently loses the review step. Check the `system`/`init` log
  line's `skills`/`plugins` fields (confirms `worker/skills/
  agent-fleet-planning` loaded) and `tool_use`/`tool_result` entries
  naming `Task`.
- **A `tool_use` entry naming `AskUserQuestion`** — this one is now
  expected and correct, unlike before `docs/adr/0018`: `AskUserQuestion`
  is a real MCP tool, proxied sidecar → `core`, answered via the
  dashboard (not Discord). If it looks stuck, check the dashboard's
  pending-questions view, not the Discord thread.
- **A worker pod exists but the sidecar container never went Ready** →
  the agent session will hang on its very first MCP tool call (no local
  MCP server to connect to). Check `kubectl describe pod` for the
  `sidecar` container's readiness/restart state before assuming the
  planning logic itself is stuck.
