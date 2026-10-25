# ADR-0070 — Headless invoke: any session can ask, one structured result

- **Status:** Proposed
- **Date:** 2026-10-20, amended 2026-10-25 (the owner's rule: "absolutely
  any session, headless or not, must be able to ask a question")
- **Built:** the proto (`Invoke`, `WaitForResult`) and their authz rows only.
- **Amends:** [ADR-0067](0067-shared-and-private-sessions.md)
  (`interaction` goes), [ADR-0066](0066-agent-service.md) §10 (a pod that
  exits with a deferred decision is parked, not crashed).
- **Related:** [ADR-0066](0066-agent-service.md) (the client principal,
  `Invoke`/`WaitForResult` in the authz table, request claims, who may
  answer), [ADR-0062](0062-defined-agents.md) §7 (`output` was left out of
  the P1 spec), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (`git.mode: push`), [ADR-0050](0050-a-question-outlives-its-pod.md)
  (a question outlives its pod), [ADR-0076](0076-definition-sessions-run-in-auto.md)
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
  (`result:<session>`, which the sidecar's `/push` must pass through; it
  sends none today), retries until core acknowledges it, and only then
  exits.
- **Which turn is final.** In a single-shot session:
  - a turn that ends deferred (§6) is not final, and any structured output
    it carries is discarded;
  - a `success` turn is final, with `done`;
  - any other subtype (`error_max_turns`, `error_during_execution`, …) is
    final, with `failed`.
- **Core alone decides the terminal state**, in this order: `expired` (§6),
  then the final result's `done`/`failed`, then a pod that exited with an
  open deferred decision is **parked** (not an end), then a pod that exited
  with neither is `AGENT_CRASHED` (ADR-0066 §10). On a final result core
  marks the session finished, archives it, and runs §7.

### 4. `WaitForResult` is a long poll

`WaitForResult(thread_id, wait_seconds ≤ 60)` returns `{state,
structured_output, result_text, subtype, error, pending}`, where `state` is:

- `done` or `failed`: the final result entry exists (§3), or the session
  ended without one (`failed`, with the ADR-0066 §10 code);
- `waiting`: an unanswered question or permission exists, pod or not.
  `pending` lists each one's `seq`, kind and text, so the client can show it
  and answer it (§5);
- `expired`: a decision stayed unanswered past `limits.answerTimeout` (§6);
- `running`: anything else, including the wait running out.

`Invoke` answers a full fleet with a typed `CAPACITY` **and** the
`thread_id`, and the claim stays pending; the SDK helpers (`invokeAndWait`,
`InvokeAndWait`) retry `Invoke` on `CAPACITY`, loop on `WaitForResult`
otherwise until `done`, `failed`, `expired` or the caller's deadline, and
hand `waiting` to the caller. The proto gains `pending`, the two states and
`thread_id` on the `CAPACITY` path (additive).

AgentService's thread `Status` reads `waiting` whenever a decision is
unanswered, with or without a pod (`status.go` reads `idle` without one
today). The dispatcher's wake gate is unchanged.

### 5. Who answers

- **The client relays.** It answers its own untainted thread through
  AgentService `AnswerQuestion`/`RespondToPermission`, as ADR-0066 §4
  allows, and names the person it relays for: an `actor` from its
  `actors.allow`, validated as `Open` validates one (ADR-0066 §6). This is
  the client's attestation, recorded with the answer; core cannot check it,
  as it cannot check any client's actor. A tainted thread is admin-only, as
  today.
- **The console.** A platform-admin answers any session, as for every other
  blocked session; Discord's "needs a human" post already links it.

Nothing answers on a person's behalf when they are silent: `expired` ends
the run instead (§6).

### 6. One parking mechanism: the SDK's deferred tool call

In a single-shot session every decision, question or permission, parks
through the SDK's own **defer** (Claude Code hooks, "Defer a tool call for
later"): a `PreToolUse` hook returns `permissionDecision: "defer"`, the tool
does not run, the turn ends with `stop_reason: "tool_deferred"` and
`deferred_tool_use {id, name, input}`, and the call stays pending in the
session. Resuming the session **with no prompt** fires `PreToolUse` again
for that same call, and the hook's answer then applies.

- **Asking.** The worker's `PreToolUse` hook defers a call that would ask:
  one `canUseTool` would ask about (it appends the `permission_request`, with
  the tool-use id, first), and the sidecar's `AskUserQuestion` (the question
  is posted first, without blocking). The turn ends; the worker exits; the
  pod goes. A parked run holds no slot and no token.
- **Answering.** An answer warms the session first and appends second, as
  `AnswerQuestion` does today; a warm that fails refuses the answer, and
  the decision stays open. The new worker waits for the answer on its
  stream, resumes the SDK session with no prompt, and the hook returns
  `allow` or `deny` for the same call. For a question, `allow` lets the
  sidecar tool return the stored answer.
- **What this does not change.** The same call runs, so no retry has to
  match it. No grant is stored, so the answerer's scope (ADR-0076 §3) is
  never widened. `ReserveSlot`'s stale-close skips deferred requests (they
  are bound to no pod) and still closes a live pod's interrupted ones.
  Resident threads keep asking live through `canUseTool`, as now.
- **Limits.** The SDK ignores `defer` when the model makes several tool
  calls in one turn; that ask goes through `canUseTool` live, holds the pod
  until it is answered or the pod's lifetime cap, and is announced like any
  other. A resume that carries a prompt drops the deferred call, so nothing
  is sent to a parked session but its answer.
- **Expiry.** `limits.answerTimeout` (a duration; single-shot sessions only;
  default 24h, at most `SESSION_RETENTION`) bounds how long a decision may
  wait. Past it, core ends the session `expired` and runs §7; the check
  rides `sessions.Loop`. A parked session whose definition changed since its
  pin cannot warm, so core ends it `failed` instead of leaving it waiting.
  Nothing is answered for anyone.
- **Spike before build.** The docs describe `defer` for `claude -p`; the
  worker runs `query()` in streaming-input mode. First confirm there that a
  deferred turn ends with `tool_deferred`, that a resume with no prompt
  fires `PreToolUse` for the same call, and that `outputFormat` does not
  demand structured output from a deferred turn. If one fails, this section
  goes back to the owner before any code.

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
  slot (five in all) doing nothing.
- **Reap the pod and replay the answer to the next one** (a grant or a
  one-time allow, then a retry). The agent must re-issue a matching call, a
  grant widens a "once", some asks are never granted (`rm`/`sudo`, plan
  exits, script writes), and a live pod's permission would need the
  stale-close deleted. The SDK's own defer re-runs the same call.
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
- An answer costs one warm, and the run continues with the very call it
  parked on.
- A full fleet refuses an answer; the client retries it, and the request
  stays open meanwhile. Answers do not jump ahead of new invokes.
- Repeating an `Invoke` with the same key is safe and returns the same
  answer, parked or not.
- A single-shot session's working tree is gone after the run. Anything worth
  keeping must be in the structured result or pushed (`git.mode: push`,
  ADR-0065).
- QA:
  - `curl` an `Invoke` and get schema-valid JSON.
  - Park more invokes than `MAX_EXTERNAL_SESSIONS` at once on questions and
    permissions. Check that each pod exits once its turn defers, that each
    run reads `waiting`, and that answering all of them finishes each one
    `done`.
  - Leave one past `answerTimeout`: it reads `expired`, with its PVC gone.
  - Answer a permission while the fleet is full: the answer is refused,
    and the request stays open.
