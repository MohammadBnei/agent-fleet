# ADR-0070 — Headless invoke: deny instead of ask, one structured result

- **Status:** Proposed
- **Date:** 2026-10-20
- **Related:** [ADR-0066](0066-agent-service.md) (the client principal,
  `Invoke`/`WaitForResult` in the authz table, request claims),
  [ADR-0067](0067-shared-and-private-sessions.md) (`interaction`,
  `lifecycle`, the worker's `AgentSpec`), [ADR-0062](0062-defined-agents.md)
  §7 (`output` was left out of the P1 spec),
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (`git.mode: push`, already decided there),
  [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md).

## Context

App code needs to call a definition the way it calls a function: input in,
typed output back, nobody watching. The plan's P7 consumer does exactly
that, through the Go or TS SDK.

A normal session cannot serve that call:

- It asks a human whenever a tool needs a decision, and waits forever when
  nobody answers.
- It runs until someone stops it.
- It ends in prose, not a value. Today's `result` transcript entry carries
  only the subtype, usage and cost, not even the final text
  (`worker/src/session.ts:537-595`).

ADR-0062 left `output` out of the P1 spec, and `interaction` and `lifecycle`
are inert (ADR-0062 §7).

## Decision

### 1. `Invoke` is `Open` with fixed choices

`Invoke(definition, input, idempotency_key)` creates a `private` session
owned by the client, with `interaction: deny` and `lifecycle: single-shot`.
Both must be in the definition's `allow` lists, or the call gets
`FailedPrecondition`. Only a client may call it (ADR-0066 §4); a member
cannot.

- **Idempotency** is ADR-0066 §7's claim on `(invoker, idempotency_key)`. A
  retry of a finished request returns its session and result and starts
  nothing. A retry of a request that failed before its input was appended
  (capacity, a definition refusal, a crash) re-runs on the same session. The
  same key with a different definition or input gets `FailedPrecondition`.
- **Warm.** Through `WarmIfIdle`, like every other path. The input is
  appended after the warm, with the key as its transcript key.

### 2. Deny, never ask

Under `interaction: deny`:

- `canUseTool` denies everything it would ask about (ADR-0067 §4: calls
  outside `tools.allow` are already denied, and a Bash match asks), with a
  message the agent can read ("not allowed in a headless run").
- `AskUserQuestion` is not registered, and core refuses its method, even
  when `sidecarTools` lists it. The agent has no way to wait on a human.
- Plan mode is refused: `ExitPlanMode` would need an approval nobody gives.

### 3. One structured result, stored before the worker exits

- **`output.schema`** joins the spec: a JSON Schema, validated at load
  (compiles, `type: object` at the root, at most 64 KiB). It applies only to
  `single-shot` sessions. A definition that allows `single-shot` without
  `output.schema` gets free-text results.
- The worker passes it to `query()` as `outputFormat: {type: "json_schema",
  schema}`. The SDK's `result` message carries `structured_output`, the
  final text and a `subtype` (`success`, `error_max_turns`,
  `error_during_execution`, …).
- **The `result` entry gains `result_text` and `structured_output`.** The
  worker writes it through `SendMessage` with a fixed idempotency key
  (`result:<session>`), retries until core acknowledges it, and only then
  exits. A lost push is retried, not dropped.
- **Core finishes the session only on a stored result.** When the result
  entry lands, core marks the session finished, archives it, and runs the
  teardown. A worker that exits with no stored result is `AGENT_CRASHED`
  (ADR-0066 §10).
- `limits.timeout` and `limits.maxTurns` still bound the run.

### 4. `WaitForResult` is a long poll

`WaitForResult(session_id, wait_seconds ≤ 60)` returns `{state,
structured_output, result_text, subtype, error}`:

- as soon as the session's `result` entry exists (`state: done`);
- when the session has ended without a result (`state: failed`, with the
  ADR-0066 §10 code). A `CAPACITY` refusal is not an end: the claim stays
  pending and the call answers `running` until the client's retry warms it;
- when the wait runs out (`state: running`).

The SDK helpers (`invokeAndWait`, `InvokeAndWait`) loop on it until a
terminal state or the caller's deadline.

### 5. Headless sessions are torn down on every terminal path

A `single-shot` session's PVC and vault token go when the session reaches
any terminal state: result stored, crash, timeout, or `Stop`. They use the
same teardown as today, so no path leaves a disk or a token behind. The
transcript stays in Postgres, so the console can still show the run. There
is no per-definition `retention` setting until a consumer needs one.

## Alternatives considered

- **A separate job runner.** A second execution path would need its own
  pod profile, vault wiring and context packs. A single-shot session gets
  all of them through `WarmIfIdle`.
- **Ask, with a timeout.** It turns a missing permission into a slow
  failure, and it invites an "auto-allow on timeout" later. Deny is
  immediate and readable by the agent.
- **Push the result to a callback URL.** That gives core an outbound
  request to an address a client supplies. A long poll keeps every
  connection inbound.

## Consequences

- A headless definition must be written so that its allow list covers its
  task. A Bash prefix rule runs only for calls the argument check passes
  (ADR-0067 §4); anything else is denied.
- Repeating an `Invoke` with the same key is safe and returns the same
  answer.
- A single-shot session's working tree is gone after the run. Anything worth
  keeping must be in the structured result or pushed (`git.mode: push`,
  ADR-0065).
- The plan's P4 QA gate covers this path: `curl` an `Invoke`, get
  schema-valid JSON, and run six sequential invokes then a seventh with no
  wedge.
