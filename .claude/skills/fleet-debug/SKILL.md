---
name: fleet-debug
description: Diagnose a stuck or failed agent-fleet task — task/journal state, worker pod logs, the Redis planning transcript, and known failure modes. Use when a Discord task thread has gone quiet, a task never opened a PR, or a worker looks stuck/crashed.
user-invocable: true
allowed-tools:
  - Read
  - Bash(psql *)
  - Bash(kubectl logs *)
  - Bash(kubectl get *)
  - Bash(kubectl describe *)
  - Bash(redis-cli *)
---

# /fleet-debug — diagnose a stuck or failed task

Work outward from the database, then the transcript, then live pod logs.
Full flow reference: `docs/ARCHITECTURE.md` §2.

## 1. Check task state first

```sql
SELECT id, repo, status, claimed_by, pr_url, updated_at
FROM tasks WHERE id = '<taskId>';
```

- `pending` and never claimed → the target worker pod may be down, or
  `TARGET_REPO` on that pod doesn't match `repo` here (check
  `k8s/<repo>-worker.yaml`'s `env`).
- `claimed`/`planning` and `updated_at` is stale (no recent writes) → the
  worker pod likely crashed or restarted mid-task. Nothing auto-requeues
  it (see `docs/adr/0003`'s Consequences) — this needs a manual status
  reset or a fresh task.
- `failed` → the error is in the Discord thread's last message and in
  `knowledge_journal` (below).

## 2. Read the journal — the real signal, not just status

```sql
SELECT event_type, payload, created_at
FROM knowledge_journal
WHERE payload->>'taskId' = '<taskId>'
ORDER BY created_at;
```

Look specifically at `session.result` events' `permissionDenials` and
`numTurns`/`totalCostUsd`. **A high `permissionDenials` count with very few
turns is the signature of a missing `allowedTools` entry** — a headless
`query()` has no `canUseTool` prompt, so a disallowed tool call is silently
denied, not errored loudly (`docs/adr/0008`). If a critic or proposer never
posted to the transcript at all despite running for a while, check this
before assuming the model just didn't respond.

## 3. Check the worker pod's live logs

```
kubectl logs -n agent-fleet deploy/<repo>-worker -f
```

Every SDK message is logged, not just the final result
(`logSdkMessage` in `worker/src/planning.ts`) — `tool_use` and
`tool_result` entries (with `isError`) show up here in real time, so a
permission denial or a crashed tool call is visible immediately rather
than reconstructed after the fact from cost/turn-count.

## 4. Inspect the Redis transcript directly

```
redis-cli -h <REDIS_HOST> -a <REDIS_MAIN_PASSWORD> LRANGE agentfleet:planning:<taskId> 0 -1
```

Each entry is `{from, text, type?, ts}`. If Discord shows the thread went
quiet but this list is still growing, the bot's relay
(`bot/src/redis.ts`/`relayHumanMessage`) or the worker's Discord posting
(`postReply`) is the broken link, not the planning logic itself.

## Known failure modes

- **Missing `allowedTools` entry** → silent permission denial, burned
  turns, zero transcript output. Fix: add the tool to the relevant
  `allowedTools`/`REDIS_MCP_TOOLS` list in `worker/src/planning.ts` for
  **every** phase that needs it (see `/fleet-feature`).
- **`PLANNING_TIMEOUT_MS=0` (the default) + a broken Discord relay** →
  `waitForCheckpointReply` blocks forever waiting for a human reply that
  never arrives in Redis. Check the bot's own connectivity/logs, not just
  the worker, before assuming planning itself hung.
- **Worker pod crash mid-task** → `removeWorktree`'s `finally` block never
  runs if the pod itself is killed (not just the `query()` call erroring),
  so the worktree/branch can be left behind in that pod's `/workspace` PVC.
  Check `git worktree list` inside the pod if a retried task fails to
  create a duplicate worktree.
- **`total_cost_usd` looks wrong/zero** → expected under subscription auth
  (`CLAUDE_CODE_OAUTH_TOKEN`, not a metered API key) — it's a notional
  figure, not a real charge (`docs/adr/0004`). Not a bug by itself.
