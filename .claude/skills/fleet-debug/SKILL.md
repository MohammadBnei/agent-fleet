---
name: fleet-debug
description: Diagnose a stuck or failed agent-fleet session — session row and pod_phase, the transcript, worker/sidecar/provisioner logs, Agent Vault and GitHub App credentials, and known failure modes. Use when a session has gone quiet, never got a pod, never opened a PR, or a worker looks stuck/crashed.
user-invocable: true
allowed-tools:
  - Read
  - Bash(psql *)
  - Bash(kubectl logs *)
  - Bash(kubectl get *)
  - Bash(kubectl describe *)
---

# /fleet-debug — diagnose a stuck or failed session

Work outward: the session row, then its transcript, then live pod and
provisioner logs. Topology: `docs/ARCHITECTURE.md` §2.

What exists (`docs/adr/0048`): a **session** is the durable unit (`sessions`
row + `transcript`); a **pod** is ephemeral compute attached by a warm. There
is no queue, no status enum, no lease heartbeat, no retry, no worktree, no
sandbox pod. A session's worker is one single-shot Kubernetes `Job`,
`worker-<shortId>` (`WorkerResourceName` in
`provisioner/internal/k8s/names.go`: the UUID without dashes, first 20
chars), with two containers, `worker` and `sidecar`.

Connect with the `AGENTFLEET_DB_*` credentials (Infisical, `agent-fleet-nygh`);
only `core` holds them.

## 1. The session row

```sql
SELECT id, repo, definition_name, pod_phase, pod_message, last_error,
       permission_mode, activity_seen, last_active_at, last_entry_type,
       stop_requested_at, swept_at, archived_at, fleet_version
FROM sessions WHERE id = '<sessionId>';
```

`pod_phase` is written from two directions: the provisioner pushes pod events
(`ReportPodEvents`), and core's 60s `sessions.Loop`
(`core/internal/sessions/reconcile.go`) re-reads what Kubernetes actually has
and corrects a missed push. The same pass runs the sweeps, each of which tears
the pod down (the session survives; the next message warms a new pod):

- **stop grace** — a Stop the pod did not honour within `STOP_GRACE_MS`;
- **startup stall** — a pod that came up and wrote nothing within
  `STARTUP_STALL_MS` (`activity_seen` false; `docs/adr/0040`);
- **idle timeout** — no transcript activity for `IDLE_TIMEOUT_MS` (a
  definition's `limits.timeout` overrides it, `docs/adr/0065`);
- **retention** — disk of sessions untouched for `SESSION_RETENTION`
  (`swept_at` set).

Defaults are in `core/internal/config/config.go`. Reading the row:

- `pod_phase` NULL and the human sent a message → the warm failed. The
  console showed the error to whoever sent it; core's logs have it. A warm
  refused at capacity says `live sessions — stop one first`
  (`MAX_IN_FLIGHT_TASKS`, counted by `livePhases` in
  `core/internal/sessions/store.go`).
- `pod_phase` live but nothing moves → §3 (pod logs), then §5.
- `pod_message`/`last_error` hold what the provisioner or a sweep last said.

"Live" (`blocked`, `working`, …) is derived, not stored
(`core/internal/sessions/liveness.go`): a live `pod_phase` plus an
unanswered `permission_request` or `question` is **blocked** — waiting on a
human click in the console, not stuck.

## 2. The transcript — what the agent actually did

```sql
SELECT seq, "from", type, left(text, 200), reply_to_seq, created_at
FROM transcript WHERE session_id = '<sessionId>' ORDER BY seq;
```

Every SDK message the worker sees is written here (`logSdkMessage` in
`worker/src/session.ts`), so this is the full record: `system` entries
(the `init` line with model, tools, skills, MCP servers; `session_failed`;
`context_loaded`), `assistant`/`user` turns, `tool_call` telemetry,
`permission_request`/`permission_response` pairs (correlated by
`reply_to_seq`), `question`/`answer`, `result`.

- A `permission_request` with no `permission_response` → blocked on a human.
- A `system` entry with `"sdk":"session_failed"` → the worker threw outside
  the SDK loop; its text is the error.
- Turns burned with nothing useful → read the `init` entry: a missing MCP
  server or skill shows there.

`knowledge_journal` is agent knowledge (`journal_write`), not pod telemetry:
pod events are not journaled (`docs/adr/0055`); their history is in Loki.

## 3. Worker and sidecar logs

```
kubectl -n agent-fleet get job,pod -l agent-fleet.dev/task-id=<sessionId>
kubectl -n agent-fleet logs job/worker-<shortId> -c sidecar
kubectl -n agent-fleet logs job/worker-<shortId> -c worker
kubectl -n agent-fleet describe pod -l agent-fleet.dev/task-id=<sessionId>
```

(The label is still called `task-id`; its value is the session id.)

- The sidecar is a native sidecar (init container with `restartPolicy:
  Always`); the worker starts only after its `/readyz` passes. A sidecar
  that never goes Ready means a worker that never starts — check
  `describe` for its probe and restart state.
- The sidecar logs every gRPC call it proxies to core. A worker making tool
  calls that change nothing usually means sidecar → core is broken
  (`FLEET_PROVISIONER_TOKEN`/lease errors are `PERMISSION_DENIED`, never
  retried).
- No container logs, pod gone → the console's log drawer reads Loki; the
  `Job` is deleted on teardown and takes its logs with it.
- A container that just stops logging → likely a cgroup OOM kill: confirm
  with the node's `dmesg` (`docs/verification-traps.md`).

## 4. The provisioner

```
kubectl -n agent-fleet logs deploy/provisioner --since=30m
```

It creates the Job and the per-session PVC, keeps the read-only repo cache
fresh, syncs `fleet-shared/` and the session's `/genome` (context packs,
`docs/adr/0063`), mints vault tokens, and reports pod events. A warm that
fails before any pod exists fails here: clone/fetch of the cache, PVC
creation, a vault refusal (§5), or the Job create itself.

## 5. Credentials: Agent Vault and the GitHub App

No pod holds a real GitHub or Anthropic token (`docs/adr/0065`,
`docs/adr/0075`). The worker has placeholders; the vault injects the real
value on the wire.

- **The GitHub App** (`provisioner/internal/github/app.go`): the provisioner
  alone holds the key (files under `GITHUB_APP_DIR`), mints 1h installation
  tokens and re-mints with <30m left. Only installations by accounts in
  `FLEET_GITHUB_ACCOUNTS` count. Log lines start with `github app:` —
  `re-mint failed, serving the last token`, `installation by a
  non-allowlisted account, ignored`, `no token for an owner, its git falls
  back to ambient auth`.
- **Repo pods** use their owner's vault (`owner-<login>`), refreshed by the
  provisioner on its own 5-minute tick (`grpcserver: owner vault refresh`
  errors). In the pod, `HTTPS_PROXY` is the sidecar's egress router
  (`127.0.0.1:9093`, `sidecar/internal/egress`): only `github.com`,
  `api.github.com` and `api.anthropic.com` go to the vault; everything else
  goes direct. `git push`/`gh` failing with auth errors while other HTTPS
  works points here. Commits are authored `FLEET_GIT_AUTHOR_*` (`ukubi`).
- **Definition pods** use their definition's vault, reconciled from
  `definitions/` every sync (`ReconcileVaults`) and again at warm. A missing
  credential key fails the warm closed, naming the key. Core also refuses a
  definition warm when the provisioner did not apply the definition profile,
  did not mint a vault token, or did not apply the context packs
  (`core/internal/dashboard/server.go`, `WarmIfIdle`). Egress is locked by
  `k8s/provisioner/definition-egress.yaml`: anything but the vault proxy,
  core :9090 and DNS is dropped.
- A token is per warm, capped at 12h with the pod (`activeDeadlineSeconds`),
  and revoked on teardown (`vault revoke failed` lines are bounded by its
  TTL).

## Known failure modes

- **Session blocked, nobody answered** → not a bug. The console lists it
  under needs-you; answer it there.
- **Message appended but the pod never reacts** → check the ordering
  invariant: warm first, then append (`resumeFromSeq` = `LatestSeq` at
  dispatch). A message appended before the pod existed sits below its cursor.
- **Pod finished but `pod_phase` still live** → the 60s reconcile corrects
  it; if it does not, core cannot list pods from the provisioner (core logs).
  A stuck live phase holds a slot until then.
- **Worker exited non-zero** → the `session_failed` transcript entry; the
  Job phase is `Failed`.
- **`total_cost_usd` looks wrong/zero** → expected under subscription auth
  (`CLAUDE_CODE_OAUTH_TOKEN`), a notional figure (`docs/adr/0004`).
- **A definition session prompts or denies something a repo session would
  not** → expected: its tools are its definition's `tools.allow` inside a
  fixed ceiling, decided by `definitionDecision` in `worker/src/session.ts`
  (`docs/adr/0076`); a grant can widen it per session or per definition.
