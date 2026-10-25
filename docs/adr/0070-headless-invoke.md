# ADR-0070 — Headless invoke: any session can ask, one structured result

- **Status:** Proposed
- **Date:** 2026-10-20, amended 2026-10-25 (the owner's rule: "absolutely
  any session, headless or not, must be able to ask a question")
- **Built:** the proto (`Invoke`, `WaitForResult`) and their authz rows only.
- **Related:** [ADR-0066](0066-agent-service.md) (the client principal,
  `Invoke`/`WaitForResult` in the authz table, request claims, who may
  answer), [ADR-0067](0067-shared-and-private-sessions.md) (`interaction`,
  `lifecycle`, the worker's `AgentSpec`), [ADR-0062](0062-defined-agents.md)
  §7 (`output` was left out of the P1 spec),
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (`git.mode: push`), [ADR-0050](0050-a-question-outlives-its-pod.md)
  (a question outlives its pod), [ADR-0076](0076-definition-sessions-run-in-auto.md)
  §3 (grants), [ADR-0078](0078-core-on-the-bridge.md) (announcements).

## Context

App code needs to call a definition the way it calls a function: input in,
typed output back. The plan's P7 consumer does exactly that, through the Go
or TS SDK. A game client ordering agents (mountain R11) is a second one.

A normal session cannot serve that call:

- It runs until someone stops it.
- It ends in prose, not a value. Today's `result` transcript entry carries
  only the subtype, usage and cost, not even the final text
  (`worker/src/session.ts`, the `result` push).

The first draft of this ADR answered "nobody is watching" with
`interaction: deny`: never ask, deny every decision. The owner rejected that
on 2026-10-25: any session must be able to ask. A headless run that hits a
decision parks until someone answers, then runs on to its result.

Most of what parking needs exists already:

- **A question outlives its pod.** It is a durable transcript row. The idle
  sweep reaps a pod waiting on one on purpose (`ListIdleSessionIDs`), and an
  answer warms the session and is replayed to the next pod
  (`AnswerQuestion`, both services).
- **Both answer paths exist.** A client answers its own untainted threads
  through AgentService; a platform-admin answers any session from the
  console (`canAnswer`, `DashboardService` is admin-only and lists every
  session).
- **Announcements already ignore the pod.** `session.waiting`/`resumed`
  follow the count of unanswered questions and permissions, pod or not
  (ADR-0078).

What does not exist: a **permission** is bound to one live pod's
`canUseTool` promise. The idle sweep therefore never reaps a pod waiting on
one, so a forgotten permission holds one of five fleet slots until the 12h
lifetime cap, and `ReserveSlot` denies it at the next warm.

## Decision

### 1. `Invoke` is `Open` with fixed choices

`Invoke(definition, input, idempotency_key)` creates a `private` session
owned by the client, with `lifecycle: single-shot`. `single-shot` must be in
the definition's `lifecycle.allow`, or the call gets `FailedPrecondition`.
Only a client may call it (ADR-0066 §4); a member cannot.

- **Idempotency** is ADR-0066 §7's claim on `(invoker, idempotency_key)`. A
  retry of a finished request returns its session and result and starts
  nothing. A retry of a request that failed before its input was appended
  (capacity, a definition refusal, a crash) re-runs on the same session. The
  same key with a different definition or input gets `FailedPrecondition`.
  A retry of a parked request returns it parked; it starts nothing.
- **Warm.** Through `WarmIfIdle`, like every other path. The input is
  appended after the warm, with the key as its transcript key.

### 2. Every session can ask; `interaction` goes

- A headless session asks the way every session does: `AskUserQuestion`
  (the sidecar's, when `sidecarTools` lists it) and `canUseTool`'s asks.
- **`interaction` is deleted** from the definition schema, the loader, the
  worker's `definitionDecision`, `answerable`'s "this thread takes no
  answers" refusal and `Open`'s check. Nothing reads it today that a human
  set: core never sends it to the worker (`agentSpec()` leaves it empty),
  so the worker's deny branch is already dead. Proto fields stay, documented
  as ignored (`buf breaking`); `sessions.interaction` keeps its column and
  is no longer written.
- A definition that must not wait on a person keeps that out of its
  `tools.allow` design, not out of the runtime: a call it allows never asks
  (ADR-0076 §2).

### 3. One structured result, stored before the worker exits

- **`output.schema`** joins the spec: a JSON Schema, validated at load
  (compiles, `type: object` at the root, at most 64 KiB). It applies only to
  `single-shot` sessions. A definition that allows `single-shot` without
  `output.schema` gets free-text results.
- The worker passes it to `query()` as `outputFormat: {type: "json_schema",
  schema}`. The SDK's `result` message carries `structured_output`, the
  final text and a `subtype` (`success`, `error_max_turns`,
  `error_during_execution`, …).
- **The final `result` entry gains `result_text` and `structured_output`.**
  The worker writes it through `SendMessage` with a fixed idempotency key
  (`result:<session>`), retries until core acknowledges it, and only then
  exits. A lost push is retried, not dropped.
- **A turn that ends on an open decision is not the end.** The worker writes
  the final result only for a `success` turn with no unanswered question or
  permission. Otherwise it stays resident, as every session does, and the
  idle sweep may reap the pod (§6).
- **Core finishes the session only on a stored final result.** When it
  lands, core marks the session finished, archives it, and runs the
  teardown. A worker that exits with no stored result is `AGENT_CRASHED`
  (ADR-0066 §10).
- `limits.timeout` (idle) and `limits.maxTurns` still bound the run.

### 4. `WaitForResult` is a long poll

`WaitForResult(session_id, wait_seconds ≤ 60)` returns `{state,
structured_output, result_text, subtype, error, pending}`, where `state` is:

- `done`: the final result entry exists;
- `waiting`: an unanswered question or permission exists, pod or not.
  `pending` lists each one's `seq`, kind and text, so the client can show it
  and answer it (§5);
- `failed`: the session ended without a result, with the ADR-0066 §10 code;
- `expired`: a decision stayed unanswered past `limits.answerTimeout` (§6);
- `running`: anything else, including the wait running out. A `CAPACITY`
  refusal is not an end: the claim stays pending and the call answers
  `running` until the client's retry warms it.

The SDK helpers (`invokeAndWait`, `InvokeAndWait`) loop on it until a
terminal state (`done`, `failed`, `expired`) or the caller's deadline, and
hand `waiting` to the caller.

AgentService's thread `Status` reads `waiting` whenever a decision is
unanswered, with or without a pod (`status.go` reads `idle` without one
today). The dispatcher's blocked gate reads the same count.

### 5. Who answers

Unchanged rules, both paths:

- **The client relays.** It answers its own untainted thread through
  AgentService `AnswerQuestion`/`RespondToPermission`, as ADR-0066 §4
  already allows. The answer is authored `client:<name>`. A tainted thread
  is admin-only, as today.
- **The console.** A platform-admin answers any session, the way they
  answer every other blocked session, and Discord's "needs a human" post
  already links it.

No new answer path and no auto-answer: nothing answers on a person's behalf
when they are silent (`expired` ends the run instead, §6).

### 6. One parking mechanism: a decision never holds a pod

For every **definition session**, a permission parks the way a question
already does:

- **The idle sweep reaps a pod waiting on a permission**, as it already does
  for a question, after the definition's `limits.timeout`. A parked session
  holds no slot, so the pool needs no per-client parking cap: a client's
  parked sessions are rows, bounded by `maxClientThreads` (500), and its
  live ones by `MAX_EXTERNAL_SESSIONS`.
- **An answer to a permission whose pod is gone still counts.** An allow is
  stored as a session grant for that exact call (ADR-0076 §3's exact rule
  form), the decision is appended, and the session warms with one system
  message naming the call and the answer. The agent retries the call, and
  the grant lets it run. A deny is appended the same way, with the message.
  A pod that is still live resolves its `canUseTool` promise as today.
- **`ReserveSlot`'s stale-close stays** for a permission nobody answered:
  the new pod streams from above it and would never see it. It writes the
  system deny only when the request has no answer, which is already how it
  reads.
- **Expiry.** `limits.answerTimeout` (a duration; single-shot sessions only;
  default 24h) bounds how long a decision may wait. Past it, core ends the
  session `expired` and runs §7's teardown. Nothing is answered for anyone.

Repo sessions keep a live pod for a permission. They have no grants
(ADR-0076 §3 is definition-only), in `auto` only `rm`/`sudo` reach a person,
and someone is at the console that opened them.

### 7. Headless sessions are torn down on every terminal path

A `single-shot` session's PVC and vault token go when the session reaches
any terminal state: result stored, crash, `expired`, or `Stop`. They use the
same teardown as today, so no path leaves a disk or a token behind. A parked
session keeps its PVC (no pod, no token) until it ends, or until the
retention sweep. The transcript stays in Postgres, so the console can still
show the run.

## Alternatives considered

- **Deny, never ask** (this ADR's first draft). Rejected by the owner: a
  headless run that needs a person fails instead of asking.
- **Keep the pod while parked, with a per-client parking cap.** A pod
  waiting on a person spends a fleet slot (five in all) doing nothing. The
  question path already proved that a decision can outlive its pod.
- **Auto-answer on timeout.** Nothing decides for a silent person; `expired`
  ends the run and says so.
- **A separate job runner.** A second execution path would need its own pod
  profile, vault wiring and context packs. A single-shot session gets all of
  them through `WarmIfIdle`.
- **Push the result to a callback URL.** That gives core an outbound
  request to an address a client supplies. A long poll keeps every
  connection inbound.

## Consequences

- A headless run can wait for days without costing a pod. Its caller sees
  `waiting` and what it waits on.
- A permission answered after the pod went costs one warm and one retried
  call. The grant is session-scoped, so the same exact call later in that
  session runs without asking.
- Definitions lose `interaction`. Files that set it fail strict decoding, so
  the same PR updates `definitions/` (`opus-workspace`, `workspace-base`).
- Repeating an `Invoke` with the same key is safe and returns the same
  answer, parked or not.
- A single-shot session's working tree is gone after the run. Anything worth
  keeping must be in the structured result or pushed (`git.mode: push`,
  ADR-0065).
- QA: `curl` an `Invoke`, get schema-valid JSON. Then one that asks: it
  reads `waiting`, its pod is reaped at `limits.timeout`, an answer warms it
  and it finishes `done`. Then one left unanswered past `answerTimeout`
  reads `expired` with its PVC gone. Run six sequential invokes then a
  seventh with no wedge.
