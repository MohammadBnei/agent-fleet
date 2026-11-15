# ADR-0070 — Headless invoke: any session can ask, one structured result

- **Status:** Accepted (2026-10-25); taint (`tainted` on `Open` and `Thread`, the admin-only tainted thread) is removed by [ADR-0082](0082-decision-cards.md) §3. Amended 2026-11-15 (proposed, awaiting the owner's approval): §6 is rebuilt on ADR-0082's one-time grants and the `defer` spike is dropped; §3's result key is per result; a build order is added.
- **Date:** 2026-10-20, amended 2026-10-25 (the owner's rule: "absolutely
  any session, headless or not, must be able to ask a question"), amended
  2026-11-15 (§6 on ADR-0082, §3's key, build order)
- **Built:** the proto (`Invoke`, `WaitForResult`) and their authz rows only;
  and, through ADR-0082 slice 2, what §6 parks on: stamped requests, one-time
  grants and a decision that holds no pod.
  **Not yet built:** everything else, in the order of "Build order" below.
- **Amends:** [ADR-0067](0067-shared-and-private-sessions.md)
  (`interaction` goes), [ADR-0066](0066-agent-service.md) §10 (a pod that
  exits with an open stamped decision is parked, not crashed).
- **Related:** [ADR-0066](0066-agent-service.md) (the client principal,
  `Invoke`/`WaitForResult` in the authz table, request claims, who may
  answer), [ADR-0062](0062-defined-agents.md) §7 (`output` was left out of
  the P1 spec), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (`git.mode: push`), [ADR-0050](0050-a-question-outlives-its-pod.md)
  (a question outlives its pod), [ADR-0076](0076-definition-sessions-run-in-auto.md)
  (grants stay the answerer's choice), [ADR-0078](0078-core-on-the-bridge.md)
  (announcements), [ADR-0082](0082-decision-cards.md) §5 (nothing blocks,
  one-time grants: what §6 now stands on).

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

When this ADR was accepted, a question parked well (ADR-0050) and a
permission did not: its answer was bound to one live pod's `canUseTool`
promise, the idle sweep never reaped a pod waiting on one, and `ReserveSlot`
denied it on the next warm. §6 then proposed the SDK's `PreToolUse` `defer`
so a permission could leave its pod, behind a spike.

**ADR-0082 has since built that half another way** ("nothing blocks",
slice 2, third PR). What the code does now:

- A new worker stamps its `permission_request` `"nb": 1`
  (`worker/src/session.ts:675`, `NON_BLOCKING_STAMP`; pushed at `:1657`).
- `canUseTool` holds the call for `DECISION_WAIT_MS` (45 s), then denies it
  "Pending (request seq N) … end your turn" (`session.ts:1690-1697`). The
  decision stays open.
- An answer warms, then appends. An allow of a grantable request carries its
  decision id; the worker tells the model "Approved: retry request seq N
  once" (`session.ts:2012`), and the retried call spends the grant through
  the sidecar's `POST /grant/consume` → `CoreService.ConsumeGrant`
  (`session.ts:1312`, `:1647`; `core/internal/decisions/answer.go:312-336`),
  once, matched on session, tool and canonical input (`answer.go:325`).
- `ReserveSlot`'s stale-close, the idle sweep's guard and the no-warm answer
  apply to unstamped requests only (ADR-0082 Built line;
  `core/internal/sessions/store.go:581-637`, `:1029-1054`).

So a stamped permission already holds no pod. This amendment rebuilds §6 on
that and drops the spike.

## Decision

### 1. `Invoke` is `Open` with fixed choices

`Invoke(definition, input, idempotency_key)` creates a `private` session
owned by the client, with `lifecycle: single-shot`. `single-shot` must be in
the definition's `lifecycle.allow`, or the call gets `FailedPrecondition`.
Only a client may call it (ADR-0066 §4); a member cannot. `Open` keeps
refusing `single-shot` (`core/internal/agentsvc/open.go:101-102`): `Invoke`
is the only way in.

- **Idempotency** is ADR-0066 §7's claim on `(invoker, idempotency_key)`. A
  retry of a finished request returns its session and result and starts
  nothing. A retry of a request that failed before its input was appended
  (capacity, a definition refusal, a crash) re-runs on the same session. A
  retry of a parked request returns it parked. The same key with a
  different definition or input gets `FailedPrecondition`.
- **Warm.** Through `WarmIfIdle`, like every other path. The input is
  appended after the warm, with the key as its transcript key.
- **A cap on parked runs.** A client file may set `maxParked` (default 20):
  `Invoke` answers `PARKED_LIMIT` (not `CAPACITY`: the SDK helpers do not
  retry it) while that many of the client's single-shot sessions wait on a
  decision. A parked run holds no pod (§6), but it keeps
  its PVC on a node's disk.

### 2. Every session can ask; `interaction` goes

- Every session asks the same way: the sidecar's `AskUserQuestion` and
  `canUseTool`'s asks. The loader adds `AskUserQuestion` to every
  definition's effective `sidecarTools`, so no definition can be written
  without it.
- **`interaction` is deleted** from every reader: the worker's
  `definitionDecision` (`session.ts:464-467`), `answerable`'s "this thread
  takes no answers" refusal, `Open`'s checks and request hash,
  `CreateThread`, the dispatcher's open, and the loader, which requires it
  today (`core/internal/definitions/load.go:612-613`).
  Nothing reads it today that a person set: core never sends it to the
  worker (`core/internal/coreserver/agent_spec.go:53-55`), so the worker's
  deny branch is already dead.
- **Rollout.** For one release the loader makes `interaction` optional and
  ignores it, and the request hash uses a constant in its place, so
  in-flight claims still match. The release after drops it from
  `definitions/` and the loader; that edit changes each definition's pin,
  so it ships when no single-shot run is parked. Proto fields stay, documented as ignored;
  `sessions.interaction` is no longer written.

### 3. One structured result, stored before the worker exits

- **`output.schema`** joins the spec: a JSON Schema, validated at load
  (compiles, `type: object` at the root, at most 64 KiB). It applies only to
  `single-shot` sessions. A definition that allows `single-shot` without
  `output.schema` gets free-text results.
- **The worker learns its lifecycle.** `AgentSpec` carries `lifecycle` and
  `output_schema`. Core sends neither today: `agentSpec` leaves lifecycle
  empty on purpose (`agent_spec.go:53-55`). Core fills `lifecycle` from the
  session row (`sessions.lifecycle`, migration 000015), not from the spec.
- The worker passes the schema to `query()` as `outputFormat: {type:
  "json_schema", schema}` **only when `lifecycle` is `single-shot`** and the
  schema is set. A resident session never gets one.
- **The SDK ends every turn with schema output.** With `outputFormat` the SDK
  adds a `StructuredOutput` end-turn tool, and at the end of a turn with no
  call to it, it injects "You MUST call the StructuredOutput tool" and
  retries, else ends `error_max_structured_output_retries` (SDK 0.3.303:
  `sdk.d.ts:2149-2155`, `:5796`, `:5937`; the CLI bundled with it). That
  includes the turn that ends on a denied "Pending" call
  (`session.ts:1690-1697`). So structured output on a `result` says nothing
  about finality; core decides it (below).
- **The definition gate passes `StructuredOutput`.** A definition session
  runs with `tools: DEFINITION_BUILTIN_TOOLS` (`session.ts:450-453`, `:1564`),
  and `definitionDecision` denies anything outside the ceiling
  (`session.ts:455-470`). When `outputFormat` is set, `definitionDecision`
  allows `StructuredOutput` before the ceiling check, and the slice that adds
  `outputFormat` proves on a live pod that the `system/init` message's
  `tools` lists it and that a schema result arrives (the types do not say
  whether `tools` filters it or whether `canUseTool` sees it).
- **The `result` entry gains `result_text` and `structured_output`**, for
  every turn. The worker writes it through `SendMessage` with the
  idempotency key **`result:<session>:<n>`**, where `n` is the result's
  ordinal in this session (the count of `result` entries before it, read
  from core at start, plus one per result this pod writes). A fixed
  `result:<session>` would be wrong: a retried append with a known key
  returns the first row's seq and writes nothing
  (`core/internal/transcript/postgres.go:87-95`), so the final result after
  a parked turn would be dropped as a duplicate of the parked turn's.
- **The key needs a transport.** The sidecar's `POST /message` sends `""` as
  the key today (`sidecar/internal/localapi/server.go:211-215`). It gains an
  `idempotencyKey` body field, passed through to both `SendMessage` and
  `AppendReplyMessage`. The worker retries the result until core
  acknowledges it, and only then exits.
- **Which turn is final.** In a single-shot session:
  - a `success` turn is final, with `done`, only when the session has no
    open stamped decision (question, permission or plan) once its result is
    appended; core checks, since only core sees every open decision. A turn
    that parks is a `success` too, and its structured output is kept on its
    entry but never returned as the result;
  - any other subtype (`error_max_turns`, `error_during_execution`,
    `error_max_structured_output_retries`, …) is final, with `failed`.
- **Core alone decides the terminal state**, in this order: `expired` (§6),
  then the final result's `done`/`failed`, then a pod that exited with an
  open stamped decision is **parked** (not an end), then a pod that exited
  with neither is `AGENT_CRASHED` (ADR-0066 §10). On a final result core
  marks the session finished, archives it, and runs §7.

### 4. `WaitForResult` is a long poll

`WaitForResult(thread_id, wait_seconds ≤ 60)` returns `{state,
structured_output, result_text, subtype, error, pending}`, where `state` is:

- `done` or `failed`: the final result entry exists (§3), or the session
  ended without one (`failed`, with the ADR-0066 §10 code);
- `waiting`: an open decision exists, pod or not. `pending` lists each one's
  `seq`, kind and text, so the client can show it and answer it (§5);
- `expired`: a decision stayed unanswered past `limits.answerTimeout` (§6);
- `running`: anything else, including the wait running out.

`Invoke` answers a full fleet with a typed `CAPACITY` **and** the
`thread_id`, and the claim stays pending; the SDK helpers (`invokeAndWait`,
`InvokeAndWait`) retry `Invoke` on `CAPACITY`, loop on `WaitForResult`
otherwise until `done`, `failed`, `expired` or the caller's deadline, and
hand `waiting` to the caller. The proto gains `pending`, the two states and
`thread_id` on the `CAPACITY` path (additive; `agent.proto:291-307` has
none of them).

AgentService's thread `Status` reads `waiting` whenever a decision is
unanswered, with or without a pod (`status.go` reads `idle` without one
today). The dispatcher's wake gate is unchanged.

### 5. Who answers

- **The client relays.** It answers its own thread through AgentService
  `AnswerQuestion`/`RespondToPermission`/`ApprovePlan`, as ADR-0066 §4
  allows, and names the person it relays for: an `actor` from its
  `actors.allow`, validated as `Open` validates one (ADR-0066 §6). This is
  the client's attestation, recorded with the answer; core cannot check it,
  as it cannot check any client's actor.
- **The console.** A platform-admin answers any session, as for every other
  blocked session; Discord's "needs a human" post already links it.

Nothing answers on a person's behalf when they are silent: `expired` ends
the run instead (§6).

### 6. A decision never holds a pod (amended 2026-11-15: on ADR-0082)

In a single-shot session no open decision keeps a pod. ADR-0082 §5 already
made every decision non-blocking; a single-shot run adds only that its pod
does not wait for the next message after a turn ends.

- **One turn per pod.** A single-shot worker exits after every turn's
  `result` is acknowledged (§3), whatever core will call it. Today the
  worker runs one continuous `query()` and treats `success` as "wait for the
  next input" (`session.ts:1423`, `:2295-2303`); in a single-shot session it
  exits instead. Before exiting it flushes anything it buffers for core (the
  planned worker span recorder's spans included), then exits explicitly
  (verification trap: `process.exitCode` is not an exit).
- **A permission parks on its stamp.** The call asks, holds for
  `DECISION_WAIT_MS`, and is denied "Pending (request seq N) … end your
  turn"; the turn ends with the stamped request open; the worker writes the
  result and exits. Core sees the pod gone with an open stamped decision:
  parked.
- **The answer resumes it.** `RespondToPermission` (both services) and
  `ApprovePlan` warm first and append second (built, ADR-0082). An allow of a
  grantable request issues the one-time grant; the new pod replays the
  answer as "Approved: retry request seq N once"; the model retries; the
  retry's `canUseTool` spends the grant through `ConsumeGrant` and the call
  runs. A deny arrives as a message the same way. A warm that fails refuses
  the answer, and the request stays open.
- **A question parks as it does today** (ADR-0050, ADR-0082): `pending`
  after the wait, the turn ends, the pod exits, the answer arrives as a turn.
- **A plan.** On a fresh pod a late approval in plan mode switches mode and
  says "go ahead" (`approvePlanLate`, `session.ts:1813-1847`). If the session
  is no longer in plan mode when the answer lands, the worker can only say
  "retry once", and since `planRetry` is empty on a fresh pod
  (`session.ts:1280`, `:2009-2012`) the retried `ExitPlanMode` asks again:
  one more park. Accepted; the re-park cap below bounds it.
- **An answer must not land on a pod that is leaving.** `WarmIfIdle` is a
  no-op while `pod_phase` reads live (`core/internal/dashboard/server.go:1175`),
  and `pod_phase` is the 60 s reconcile's, so an answer appended just after a
  turn ends lands above the leaving pod's cursor and below the next pod's
  (`server.go:1237`, `resumeFromSeq` = `LatestSeq`): never delivered. So the
  worker's last result carries the seq of the last human entry it consumed,
  and when a single-shot pod ends, core re-warms the session if a human or
  answer entry lies above that seq, with the new pod's cursor just past it.
  This is the one change to `WarmIfIdle`: an explicit cursor for this path.
- **Grant matching on retry: a per-tool match key, not the defer hook.**
  `ConsumeGrant` matches the full canonical input (`answer.go:325`), and an
  input with no canon is never grantable (`decisions.go:134-137`, `:287`).
  A resumed model re-derives the call and may rewrite free text that changes
  nothing it does, Bash's `description` above all, and the grant misses.
  The fix is a match key: `decisions` stores, and `ConsumeGrant` compares,
  the canon of a per-tool subset of the input. Bash drops `description`;
  every other tool keeps its whole input until a measured miss names a
  field. The card still shows the full input, and the subset never drops a
  field that changes what runs. Both sides call the one function, as `Canon`
  does now. A request stored before the change keeps its full canon, so its
  retry misses and asks again: the safe direction.
- **What the defer hook would still add** is exact same-call replay: the
  parked `tool_use` itself runs on resume, so the model cannot rewrite or
  skip it, and no retry turn is spent. With the match key, the remaining
  gap is a model that, told "retry once", issues a materially different call
  or none. A different call is a new decision and asks, which is correct; no
  call is a run that ends on its own result, which core reports. Neither
  needs `defer`. **The spike is dropped.** It comes back only if the
  re-park counter below shows grant misses in practice, and then scoped to
  that one question: does a resume with no prompt in streaming-input mode
  fire `PreToolUse` again for the deferred call.
- **Re-park cap.** Each park costs a person an answer and the fleet a warm,
  and a retry that misses its grant parks again. A single-shot run that
  parks more than 10 times ends `failed` (`PARK_LIMIT`); the count is the
  run's stamped decisions, so it needs no new column.
- **A pod exits mid-call.** The idle sweep, a crash or the lifetime cap can
  end a pod while a tool runs or a call waits in `canUseTool`. The CLI's
  JSONL then holds a `tool_use` with no `tool_result`. ADR-0050 found the
  resume behaviour undocumented; the bundled CLI has code that finds an
  unresolved `tool_use` on resume, but nothing here relies on it. The worker's
  `released`, `spentDecisions` and `planRetry` are memory only
  (`session.ts:1268-1280`), so the next pod has none: a late approval of a
  call the dead pod held becomes "retry once" (grant still spendable, once),
  a plan approval may ask again (above). The decision row is durable, so
  nothing is lost that a person gave. **Required test** (the worker-exit
  slice): resume a session whose JSONL ends on an unanswered `tool_use`,
  check that the SDK starts, that the next turn runs, and that an open
  stamped decision for that call is still answerable and its grant spendable.
- **A parked session takes only its answer.** `WarmIfIdle` and the message
  paths (the console's Warm, `PostMessage`, a dispatcher wake) refuse a
  single-shot session with an open decision; only its answer warms it.
- **Resident threads** are unchanged: their pod stays for the next message,
  and a decision there is ADR-0082's as built.
- **Expiry.** `limits.answerTimeout` (a duration; single-shot sessions only;
  default 24h, strictly less than `SESSION_RETENTION`, and the retention
  sweep skips parked sessions) bounds how long a decision may wait. Past
  it, core ends the session `expired` (the decisions go `expired`, a state
  migration 000023 already allows) and runs §7; the check rides
  `sessions.Loop`. A parked session whose definition changed since its pin
  cannot warm, so core ends it `failed`. Nothing is answered for anyone.
- **No migration.** Parked is derived, as every state is (ADR-0048 §2):
  `lifecycle = 'single-shot'`, no live pod, an open row in `decisions`
  (migration 000023 has `state`, `created_at` and `UNIQUE (session_id,
  request_seq)`, which is all §6 reads). A slice that finds it needs a column
  adds a new numbered pair.

### 7. Headless sessions are cleaned up on every terminal path

A `single-shot` session that reaches a terminal state (final result, crash,
`expired`, `Stop`) is torn down (`TearDownSession`: pod and vault token)
and then swept (`SweepSession`: its PVC), the same calls the retention GC
makes. A parked session keeps its PVC, with no pod and no token, until it
ends. The transcript stays in Postgres, so the console can still show the
run.

## Build order

One PR each, in this order. Every slice names how it is reached and what
proves it; none ships code that nothing can call.

1. **`AgentSpec` carries `lifecycle` and `output_schema`.** Proto field adds;
   the loader validates `output.schema` (compiles, root `type: object`,
   ≤ 64 KiB); `agentSpec` fills `lifecycle` from the session row. Entry:
   `GetSession` for any definition session (resident reads `resident`).
   Test: loader table tests; `agent_spec_test.go` for both lifecycles and
   the sha. First, because every later worker slice reads it.
2. **The result entry and its key.** `POST /message` takes
   `idempotencyKey`; the worker writes `result_text` and
   `structured_output` on every `result`, keyed `result:<session>:<n>`.
   Entry: every session's result (resident too). Test: sidecar handler
   test that the key reaches core; an integration test that two results
   with distinct keys are two rows and a retried one is one.
3. **The grant match key.** `decisions` stores the per-tool subset's canon;
   `ConsumeGrant` compares it. Entry: today's resident sessions' grants.
   Test: a Bash retry with a new `description` spends the grant; one with a
   new `command` does not. Before parking, since parking makes retries
   routine.
4. **`interaction`, release one.** Loader optional and ignored, the request
   hash's constant, `AskUserQuestion` in every definition's `sidecarTools`,
   the worker's deny branch gone. Entry: the sync. Test: loader and hash
   tests; an old claim still matches. Before `Invoke`, so `Invoke` never
   reads it.
5. **`Invoke` and `WaitForResult`, with the final-turn rule.** Handlers,
   proto additions (`pending`, `thread_id` on `CAPACITY`, `waiting`,
   `expired`), §3's finality (`success` and no open stamped decision).
   Entry: `Invoke` on `:9091` for a test definition allowing `single-shot`.
   The worker still runs continuously, so a run's pod idles until the idle
   sweep: correct, only slow. Test: bufconn Invoke → WaitForResult `done`
   with `result_text`; `CAPACITY` with `thread_id`; same key twice runs once.
   This is the first slice a single-shot session can exist in, so it comes
   before `outputFormat`.
6. **`outputFormat` for single-shot only.** The worker passes it when
   `lifecycle` is `single-shot` and a schema is set; `definitionDecision`
   allows `StructuredOutput`. Entry: `Invoke`. Test: worker unit test (set
   only for single-shot; resident never); a `/kind-local` run whose
   `system/init` lists `StructuredOutput` and whose `WaitForResult` returns
   schema-valid JSON (the QA `curl`).
7. **The worker exits after each turn**, flushing first, with the consumed
   seq on its result; core's re-warm for an answer above it. Entry:
   `Invoke`. Test: worker test that a single-shot `success` exits and a
   resident one does not; integration test of an answer appended in the
   exit window reaching the next pod; the mid-call resume test (§6).
8. **§7 teardown and sweep** on final result, crash and `Stop`. Entry:
   `Invoke`. Test: fake provisioner records `TearDownSession` then
   `SweepSession` per path; a parked run keeps its PVC.
9. **Parked is respected.** `WarmIfIdle` and the message paths refuse a
   parked single-shot session except by its answer; the retention sweep
   skips it; `Status`/`WaitForResult` read `waiting` without a pod; the
   re-park cap. Entry: `Invoke`, then an answer. Test: Warm and
   `PostMessage` refused; the answer warms; the eleventh park ends `failed`.
10. **`maxParked` / `PARKED_LIMIT`.** Client file field, `Invoke` refusal.
    Entry: `Invoke`. Test: the twenty-first parked run is refused, not
    queued.
11. **`answerTimeout` and `expired`; a changed definition ends `failed`.**
    Both ride `sessions.Loop` and end through §7. Entry: a parked run left
    alone. Test: a past-timeout run reads `expired` with its PVC gone; a
    run whose definition moved reads `failed`.
12. **`interaction`, release two.** Drop it from `definitions/` and the
    loader. Ships when no single-shot run is parked (it moves every pin).

## Alternatives considered

- **Deny, never ask** (this ADR's first draft). Rejected by the owner: a
  headless run that needs a person fails instead of asking.
- **Keep the pod while parked.** A pod waiting on a person spends a fleet
  slot (five in all) doing nothing.
- **The SDK's `PreToolUse` `defer`** (this ADR as accepted on 2026-10-25,
  behind a spike). Exact same-call replay, but a second mechanism beside
  ADR-0082's grants, documented for `claude -p` rather than
  streaming-input, ignored by the SDK when a turn has several tool calls
  (so a live `canUseTool` path stays anyway), and it needs the worker to
  read its own answer before resuming. Once grants exist, the only gap it
  closes is a rewritten retry, which a match key closes for less.
- **Grant on a looser match** (tool name only, or a prefix). A grant would
  then allow calls nobody saw. The match key drops only fields that change
  nothing that runs.
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
- An answer costs one warm and one retry turn: the model re-issues the call,
  and the grant lets that one through once. A retry that changes what runs
  asks again.
- A single-shot run's pod lives for one turn. Every turn writes a result
  with structured output; only core's check makes one final.
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
    permissions. Check that each pod exits once its turn ends, that each
    run reads `waiting`, and that answering all of them finishes each one
    `done`.
  - Answer a permission whose retry rewrites Bash's `description`: it runs
    on the grant without a second ask.
  - Leave one past `answerTimeout`: it reads `expired`, with its PVC gone.
  - Answer a permission while the fleet is full: the answer is refused,
    and the request stays open.
