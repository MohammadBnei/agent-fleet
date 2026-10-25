# ADR-0070 — Headless invoke: any session can ask, one structured result

- **Status:** Proposed
- **Date:** 2026-10-20, amended 2026-10-25 (the owner's rule: "absolutely
  any session, headless or not, must be able to ask a question")
- **Built:** the proto (`Invoke`, `WaitForResult`) and their authz rows only.
- **Amends:** [ADR-0050](0050-a-question-outlives-its-pod.md) (a permission
  outlives its pod too), [ADR-0066](0066-agent-service.md) §8 (its
  permission reaping keeps the answer), [ADR-0067](0067-shared-and-private-sessions.md)
  (`interaction` goes).
- **Related:** [ADR-0066](0066-agent-service.md) (the client principal,
  `Invoke`/`WaitForResult` in the authz table, request claims, who may
  answer), [ADR-0062](0062-defined-agents.md) §7 (`output` was left out of
  the P1 spec), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (`git.mode: push`), [ADR-0076](0076-definition-sessions-run-in-auto.md)
  (grants stay the answerer's choice), [ADR-0078](0078-core-on-the-bridge.md)
  (announcements).

## Context

App code needs to call a definition the way it calls a function: input in,
typed output back. The plan's P7 consumer does exactly that, through the Go
or TS SDK. A game client ordering agents (mountain R11) is a second one.

A normal session cannot serve that call:

- It runs until someone stops it.
- It ends in prose, not a value: today's `result` entry carries the subtype,
  turns, durations, usage, cost and errors, but not the final text.

The first draft of this ADR answered "nobody is watching" with
`interaction: deny`: never ask, deny every decision. The owner rejected that
on 2026-10-25: any session must be able to ask. A headless run that hits a
decision parks until someone answers, then runs on to its result.

A **question** already parks well: it is a durable transcript row, the idle
sweep reaps a pod waiting on one on purpose, and an answer warms the session
and is replayed to the next pod as a turn (ADR-0050).

A **permission** does not. Its answer is bound to one live pod's
`canUseTool` promise, so:

- the idle sweep never reaps a pod waiting on one, and a forgotten
  permission holds one of five fleet slots until the 12h lifetime cap
  (ADR-0066 §8 already decided to reap them for member and client sessions,
  which would lose the answer);
- `ReserveSlot` writes a system deny for every unanswered permission on the
  next warm, whatever caused the warm: the system answers for a person.

## Decision

### 1. `Invoke` is `Open` with fixed choices

`Invoke(definition, input, idempotency_key)` creates a `private` session
owned by the client, with `lifecycle: single-shot`. `single-shot` must be in
the definition's `lifecycle.allow`, or the call gets `FailedPrecondition`.
Only a client may call it (ADR-0066 §4); a member cannot.

- **Idempotency** is ADR-0066 §7's claim on `(invoker, idempotency_key)`. A
  retry of a finished request returns its session and result and starts
  nothing. A retry of a request that failed before its input was appended
  (capacity, a definition refusal, a crash) re-runs on the same session. A
  retry of a parked request returns it parked. The same key with a
  different definition or input gets `FailedPrecondition`.
- **Warm.** Through `WarmIfIdle`, like every other path. The input is
  appended after the warm, with the key as its transcript key.
- **A cap on parked runs.** A client file may set `maxParked` (default 20):
  `Invoke` answers `CAPACITY` while that many of the client's single-shot
  sessions wait on a decision. A parked run holds no pod (§6), but it keeps
  its PVC on a node's disk.

### 2. Every session can ask; `interaction` goes

- Every session asks the same way: the sidecar's `AskUserQuestion` and
  `canUseTool`'s asks. The loader adds `AskUserQuestion` to every
  definition's effective `sidecarTools`, so no definition can be written
  without it.
- **`interaction` is deleted** from the worker's `definitionDecision`,
  `answerable`'s "this thread takes no answers" refusal and `Open`'s check.
  Nothing reads it today that a person set: core never sends it to the
  worker, so the worker's deny branch is already dead.
- **Rollout.** For one release the loader accepts and ignores
  `interaction`, and the request hash uses a constant in its place, so
  in-flight claims still match. The release after drops it from
  `definitions/` and the loader. Proto fields stay, documented as ignored;
  `sessions.interaction` is no longer written.

### 3. One structured result, stored before the worker exits

- **`output.schema`** joins the spec: a JSON Schema, validated at load
  (compiles, `type: object` at the root, at most 64 KiB). It applies only to
  `single-shot` sessions. A definition that allows `single-shot` without
  `output.schema` gets free-text results.
- **The worker learns its lifecycle.** `AgentSpec` carries `lifecycle` and
  `output.schema` (core sends neither today).
- The worker passes the schema to `query()` as `outputFormat: {type:
  "json_schema", schema}`. The SDK's `result` message carries
  `structured_output`, the final text and a `subtype`.
- **The final `result` entry gains `result_text` and `structured_output`.**
  The worker writes it through `SendMessage` with a fixed idempotency key
  (`result:<session>`), retries until core acknowledges it, and only then
  exits.
- **Which turn is final.** In a single-shot session:
  - a `success` turn with no unanswered question or permission is final,
    with `done`;
  - any other subtype (`error_max_turns`, `error_during_execution`, …) is
    final, with `failed`;
  - a `success` turn that ends on an open decision is not final. The worker
    stays resident, and the idle sweep may reap the pod (§6).
- **Core finishes the session only on a stored final result.** When it
  lands, core marks the session finished, archives it, and runs §7. A worker
  that exits with no stored result is `AGENT_CRASHED` (ADR-0066 §10).

### 4. `WaitForResult` is a long poll

`WaitForResult(thread_id, wait_seconds ≤ 60)` returns `{state,
structured_output, result_text, subtype, error, pending}`, where `state` is:

- `done` or `failed`: the final result entry exists (§3), or the session
  ended without one (`failed`, with the ADR-0066 §10 code);
- `waiting`: an unanswered question or permission exists, pod or not.
  `pending` lists each one's `seq`, kind and text, so the client can show it
  and answer it (§5);
- `expired`: a decision stayed unanswered past `limits.answerTimeout` (§6);
- `running`: anything else, including the wait running out. A `CAPACITY`
  refusal is not an end: the claim stays pending and the call answers
  `running` until the client's retry warms it.

The proto gains `pending` and the two states (additive). The SDK helpers
(`invokeAndWait`, `InvokeAndWait`) loop until `done`, `failed`, `expired` or
the caller's deadline, and hand `waiting` to the caller.

AgentService's thread `Status` reads `waiting` whenever a decision is
unanswered, with or without a pod (`status.go` reads `idle` without one
today). The dispatcher's wake gate is unchanged.

### 5. Who answers

- **The client relays a person's answer.** It answers its own untainted
  thread through AgentService `AnswerQuestion`/`RespondToPermission`, as
  ADR-0066 §4 allows, and **names the person**: an `actor` from the client's
  `actors.allow`, validated as `Open` validates one (ADR-0066 §6). An answer
  without one is refused, so "a person answered" stays checkable. A tainted
  thread is admin-only, as today.
- **The console.** A platform-admin answers any session, as for every other
  blocked session; Discord's "needs a human" post already links it.

Nothing answers on a person's behalf when they are silent: `expired` ends
the run instead (§6).

### 6. One parking mechanism: a decision never holds a pod

For every session, a permission parks the way a question already does, and
its answer reaches the agent the same way:

- **The idle sweep reaps a pod waiting on a permission**, after
  `limits.timeout`, as it does for a question. The sweep's permission skip
  is deleted.
- **The stale-close is deleted.** `ReserveSlot` no longer writes a system
  deny for unanswered permissions: the request stays open and answerable,
  and the session keeps reading `waiting` until someone answers it.
- **An answer warms first, then appends**, as `AnswerQuestion` does today.
  A warm that fails (capacity) refuses the answer, so a decision is never
  recorded where no pod will read it. A live pod is a no-op warm, and its
  `canUseTool` promise resolves as today.
- **The worker replays an answer it has no promise for.** A
  `permission_response` whose request the current pod never raised is
  delivered as a turn ("your request to run … was allowed / denied by …"),
  and an allow becomes a **one-time** allow for that exact call in this pod:
  the next identical call runs once without asking. Nothing is stored as a
  grant, so the answerer's choice of scope (ADR-0076 §3) is not widened.
  This covers every kind of ask, including `rm`/`sudo`, `ExitPlanMode` and
  script writes.
- **Expiry.** `limits.answerTimeout` (a duration; single-shot sessions only;
  default 24h) bounds how long a decision may wait. Past it, core ends the
  session `expired` and runs §7. Nothing is answered for anyone.

### 7. Headless sessions are cleaned up on every terminal path

A `single-shot` session that reaches a terminal state (final result, crash,
`expired`, `Stop`) is torn down (`TearDownSession`: pod and vault token)
and then swept (`SweepSession`: its PVC), the same calls the retention GC
makes. A parked session keeps its PVC, with no pod and no token, until it
ends. The transcript stays in Postgres, so the console can still show the
run.

## Alternatives considered

- **Deny, never ask** (this ADR's first draft). Rejected by the owner: a
  headless run that needs a person fails instead of asking.
- **Keep the pod while parked.** A pod waiting on a person spends a fleet
  slot (five in all) doing nothing. The question path already proved that a
  decision can outlive its pod.
- **Store a late allow as a session grant, then have the agent retry.** It
  widens a "once" into a standing rule nobody chose, cannot cover asks that
  are never granted (`rm`/`sudo`, plan exits, script writes), and loops on
  them.
- **Auto-answer on timeout.** Nothing decides for a silent person; `expired`
  ends the run and says so.
- **A separate job runner.** A second execution path would need its own pod
  profile, vault wiring and context packs. A single-shot session gets all of
  them through `WarmIfIdle`.
- **Push the result to a callback URL.** That gives core an outbound
  request to an address a client supplies. A long poll keeps every
  connection inbound.

## Consequences

- Any session can wait for days without costing a pod. A headless caller
  sees `waiting` and what it waits on.
- A permission answered after its pod went costs one warm and one turn; the
  agent decides whether to repeat the call, and if it does, it runs once.
- An open permission now survives a warm. A session can therefore show an
  old request next to a newer one; both stay answerable.
- Repeating an `Invoke` with the same key is safe and returns the same
  answer, parked or not.
- A single-shot session's working tree is gone after the run. Anything worth
  keeping must be in the structured result or pushed (`git.mode: push`,
  ADR-0065).
- QA:
  - `curl` an `Invoke` and get schema-valid JSON.
  - Park more invokes than `MAX_EXTERNAL_SESSIONS` at once on questions and
    permissions. Check that their pods are reaped at `limits.timeout`, that
    each reads `waiting`, and that answering all of them finishes each one
    `done`.
  - Leave one past `answerTimeout`: it reads `expired`, with its PVC gone.
  - Answer a permission while the fleet is full: the answer is refused,
    and the request stays open.
