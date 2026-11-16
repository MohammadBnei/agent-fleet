# ADR-0070 — Headless invoke: any session can ask, one structured result

- **Status:** Accepted (2026-10-25); taint (`tainted` on `Open` and `Thread`, the admin-only tainted thread) is removed by [ADR-0082](0082-decision-cards.md) §3. Amended 2026-11-15: §6 is rebuilt on ADR-0082's one-time grants and the `defer` spike is dropped; §3's result key is per result; a build order is added. Approved by the owner (merged as #369); the six open questions were answered 2026-11-16 (see "Owner answers" at the end of §6). All twelve build-order slices built (2026-11-16); the live `/kind-local` QA is still outstanding.
- **Date:** 2026-10-20, amended 2026-10-25 (the owner's rule: "absolutely
  any session, headless or not, must be able to ask a question"), amended
  2026-11-15 (§6 on ADR-0082, §3's key, build order)
- **Built:** the proto (`Invoke`, `WaitForResult`) and their authz rows only;
  and, through ADR-0082 slice 2, what §6 parks on: stamped requests, one-time
  grants and a decision that holds no pod; build-order slice 1 (`AgentSpec`
  carries `lifecycle` and `output_schema`: the loader validates
  `output.schema`; core fills `lifecycle` and the schema from the session row
  for `single-shot` only, so a resident session's `agent_spec_sha` is
  unchanged); slice 3, the grant match key (`decisions.MatchKey`:
  `input_canon`, `ConsumeGrant` and the `NonBlocking`/`OpenRequest` dedup all
  hold the per-tool subset's canon); slice 4 (`interaction`, release one:
  optional and ignored by the loader and every reader, a constant in the
  request hash, `AskUserQuestion` added to every effective `sidecarTools`, the
  worker's deny branch gone); slice 2 (the result entry and its key: `POST
  /message` takes `idempotencyKey`, the worker writes
  `result_text`/`structured_output`/`consumedSeq` on every result keyed
  `result:<session>:<consumedSeq>:<k>`, retried up to 5 times, the exit
  result until core acks it, slice 7); slice 5 (`Invoke` and
  `WaitForResult` in `core/internal/agentsvc/invoke.go`: Open's claim flow
  on `(invoker, idempotency_key)` with the request hash over definition and
  input, a private single-shot thread warmed through `WarmForAppend`, a full
  fleet answered `ResourceExhausted` with an `InvokeCapacity` detail
  carrying the `thread_id`; runs kept out of Open's thread cap and capped
  on their own at 500 without a result per client, under their own
  one-slot claim semaphore; `WaitForResult` a long poll on the transcript
  hub, `waiting` with `pending` on any open question, permission or plan,
  else §3's rule on the exit result, slice 7.
  `expired` is declared, reported from slice 11, built below); slice 6 (`outputFormat`:
  the worker's `outputFormatFor` sets `{type: json_schema, schema}` only for
  `lifecycle: single-shot` with `outputSchema` set, never for a resident or
  repo session, and `definitionDecision` allows `StructuredOutput` before the
  ceiling only then. Checked against SDK 0.3.325 with a local `query()`:
  `tools: [...]` does not filter `StructuredOutput` (`system/init` lists it)
  and the CLI never routes it through `canUseTool`, even a deny-all one; the
  `/kind-local` `Invoke` run in "Build order" slice 6 is **still
  outstanding**: no live pod has yet shown `system/init` listing
  `StructuredOutput` or `WaitForResult` returning schema-valid JSON through
  sidecar protojson. It needs a single-shot test definition with
  `output.schema` and a model token, and is tracked as the ADR's end-to-end
  live QA after slice 7); slice 10 (a client file's `maxParked`, a positive
  int, default 20; `Invoke` refuses a new run `ResourceExhausted` keyed
  `agent.parked_limit` while the client has that many parked runs, counted by
  `sessions.Store.ParkedRuns`: single-shot, not archived, no live pod, an
  open question, permission or plan. Its `parkedWhere` is the one SQL
  spelling of parked, for slice 9 to reuse); slice 8 (§7: each
  `sessions.Loop` tick, `endRuns` takes every single-shot run with no live
  pod that was stopped, or, with no open decision, crashed or exited with no
  input above its exit result's `consumedSeq` (slice 7), claims it by
  archiving it with that condition re-checked in the same UPDATE (so a warm
  and a claim serialize on the row), then calls `TearDownSession`, then
  `SweepSession`, then marks it swept; a parked run keeps its PVC. Invoke's
  run cap counts runs with no result entry; `WaitForResult` still reads an
  archived run for its owner, `failed` when it has no final result, and
  re-reads the row at least every 5 s while it waits); slice 7 (the worker
  exits when its input is used up: a `single-shot` worker, after a result
  with `queued_turn_count` 0 (absent is not 0), nothing pushed since or
  unpulled and no entry mid-handling, writes its last result with `exit:
  true` and `consumedSeq` (the highest human or session entry it took in;
  none is taken in once it decides), retried until core acks, flushes its
  spans and returns, and `index.ts` exits explicitly; a non-success exit
  result still exits non-zero. A turn that ends on an open decision (a
  permission it let go, a question whose tool came back pending) first holds
  up to `DECISION_WAIT_MS`; an answer in the hold runs on the same pod. A
  resident worker never exits. Core judges only exit results:
  `sessions.Store.LastExitResult` for `WaitForResult` and the same SQL
  (`exitResultOf`, `inputAfterExit`) in `endedRun`; the exit result's
  `exit`/`consumedSeq` are read from its JSON in SQL, guarded by `IS JSON`.
  A turn-starting entry is a human message, answer or permission response
  or a relayed peer prompt (`from` `session`), the worker's `takeIn` filter.
  A run with one above its exit result's `consumedSeq` is in the exit
  window: every warm of it, whoever calls `WarmForAppend`, starts the new pod
  at `consumedSeq + 1` instead of `LatestSeq` (`sessions.Store.ExitCursor`,
  read inside `warm`), and with a TERMINATED pod that nobody warmed `endRuns`
  re-warms it (`ListRewarmRuns`, through `WarmIfIdle`) and `endedRun` leaves
  it alone. An archived run with no exit result is judged on its latest
  result by the seq rule from before slice 7 (no turn-starting entry above
  its seq), so a run whose pod ran an older worker at deploy, or whose result
  had no `queued_turn_count`, still reads `done`; with no such result it reads
  `AGENT_CRASHED` on a crashed pod, `NO_RESULT` on a clean exit without a
  stop. A plan
  approval of a stamped request carries `planDecisionId` and `planInput` on
  its `permission_response`; a pod that never held the call, out of plan
  mode, says "go ahead" with no retry and lets that plan's `ExitPlanMode`
  through. The mid-call resume was checked by hand against SDK 0.3.325 with
  the real CLI: a Bash call parked in `canUseTool`, the CLI SIGKILLed, so
  the JSONL ends on a `tool_use` with no `tool_result`; a resume starts with
  the same session id, the CLI inserts a "[Tool call interrupted …]"
  `tool_result`, and the next turn runs to `success` with
  `queued_turn_count: 0`. A graceful abort instead writes its own
  `tool_result`. The decision side is the worker test "an allow for a call a
  dead pod held": the answer arrives as a message and the retry spends the
  grant. Not run live on a pod: the `/kind-local` `Invoke` QA above still
  covers it; and the re-warm runs on the 60 s tick, not on the pod event);
  slice 11 (`limits.answerTimeout`, a duration in (0, 5d], optional and
  defaulted to 24h where it is read, never written into the effective spec,
  so no pin moves; each `sessions.Loop` tick, `expireRuns` runs before
  `endRuns` over every single-shot run with no live pod, neither archived nor
  swept (`ListIdleRuns`): one with a question, permission or plan open past
  its timeout (capped at `SESSION_RETENTION`) is archived by `ExpireRun` with
  that re-checked in the UPDATE, its open decisions go `expired`; a parked
  one pinned to a sha that is not the registry's current valid head is
  archived by `EndMovedRun`, its open decisions go `stale` (an unparked run
  on a moved pin is left as it was). Both then end through slice 8's teardown
  and sweep in the same tick, and `WaitForResult` reads `expired`
  (`ANSWER_TIMEOUT`) or `failed` (`DEFINITION_CHANGED`) from those states,
  ahead of the exit result. A registry that is stale or cannot resolve the
  name ends nothing for a moved pin. A late answer to a decision so closed,
  of any kind, is refused `agent.decision_expired` or `agent.decision_stale`,
  not "already answered". `ListRetentionExpired` skips an unarchived parked
  run); 9 (parked is respected: `WarmForAppend`, so `WarmIfIdle`, the
  console's Warm and `PostMessage`, AgentService `PostMessage`,
  `PromptSession` and every other append, refuses a parked run
  (`parkedWhere`, read by `sessions.Store.ParkState`) `FailedPrecondition`
  keyed `agent.parked`; an answer warms it through `WarmForAnswer`, the one
  warm `decisions.Effects` holds, which the exit-window re-warm also uses
  since it delivers input already appended. `limits.maxParks` is a positive
  int, default 10, absent from `effective` when unset, so frozen rows read
  the default; the park count is `parkCount` in SQL, the exit results at
  whose `created_at` a question, permission or plan was open (created
  earlier, still open or answered later, or on a claimed run withdrawn before
  its `archived_at`, so the verdict outlives `ClaimEndedRun`'s withdraw), no
  column. A run over its cap with no live pod is in `endedRun` (claimed, torn
  down, swept), `WaitForResult` reads it `failed` with `PARK_LIMIT` before
  the open-decision rule, and every warm, an answer's included, refuses it.
  AgentService `GetStatus` reads `waiting` with no pod for a single-shot run
  with an open question, permission or plan (never a card alone); resident
  threads keep `idle`, as §4's "whenever" is left to a later slice);
  slice 12 (`interaction`, release two: gone from `definitions/`,
  `local/kind/definitions/` and the loader's `Spec`, so the strict decode
  refuses an `interaction:` key as unknown, and every definition's pin
  moved; a stored row whose effective still holds it has the key stripped
  before the strict decode, so it is still judged, and revoked, under this
  core's ceiling like any other row, never left alone as a newer core's.
  `Open` ignores the request's `interaction` and checks only `lifecycle`;
  the request hash keeps the `"ask"` constant, since claims are never
  deleted; `sessions.interaction` is neither written nor read (the column
  stays, NULL); `ListDefinitions` no longer fills `interaction_allow`, and a
  `Thread`'s `interaction` is empty. The proto fields stay, `[deprecated =
  true]` and documented as ignored, so `buf breaking` passes).
  **All twelve build-order slices are built.** Still outstanding: the
  `/kind-local` `Invoke` QA named in slice 6 and the QA list under
  "Consequences", on a live pod.
- **Amends:** [ADR-0067](0067-shared-and-private-sessions.md)
  (`interaction` goes), [ADR-0066](0066-agent-service.md) §10 (a pod that
  exits with an open decision is parked, not crashed; a clean exit with
  neither is `NO_RESULT`, not `AGENT_CRASHED`),
  [ADR-0082](0082-decision-cards.md) §5 (two points: a one-time grant is
  bound to a per-tool match key of the input, not the exact tool input; and
  in a single-shot session a question, permission or plan expires at
  `limits.answerTimeout`, where §5 gives `expires_at` to cards only).
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
  The dispatcher's first-responder key (ADR-0086 §3) is `alert:<groupKey>:<definition>:<window>`: one run per
  definition and cooldown window, not per group alone.
- **Warm.** Through `WarmForAppend`, like every other path. The input is
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
  session row (`sessions.lifecycle`, migration 000015), not from the spec,
  and only for `single-shot`: a resident session keeps `""` (the worker reads
  `""` as resident) and an unset schema stays empty. Deterministic proto
  encoding omits empty fields, so `agent_spec_sha` (`agent_spec.go:42-48`)
  stays byte-identical for every existing session and a live pod is never
  refused by ADR-0066 §8's gate after the deploy. A pod restart per slice
  was the alternative; it would cost every live definition session its
  turn for a field none of them uses.
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
  every turn, plus `consumedSeq` (below) and, on the pod's last result,
  `exit: true`. The worker writes it through `SendMessage` with the
  idempotency key **`result:<session>:<consumedSeq>:<k>`**, where `k` counts
  this pod's results from 1. Both parts are the worker's own: no RPC
  exposes a session's result count, and a new pod only starts after new
  input, so its `consumedSeq` differs from any earlier pod's. A fixed
  `result:<session>` would be wrong: a retried append with a known key
  returns the first row's seq and writes nothing
  (`core/internal/transcript/postgres.go:87-95`), so the final result after
  a parked turn would be dropped as a duplicate of the parked turn's.
- **The key needs a transport.** The sidecar's `POST /message` sends `""` as
  the key today (`sidecar/internal/localapi/server.go:211-215`). It gains an
  `idempotencyKey` body field, passed through to both `SendMessage` and
  `AppendReplyMessage`. The worker retries the result until core
  acknowledges it, and only then exits.
- **"Open decision" means any kind**: a question, permission or plan with
  `state = 'open'` in `decisions`. Only permissions carry the `"nb": 1`
  stamp (`core/internal/decisions/decisions.go:72-80`; a question has none),
  so nothing here tests the stamp. A single-shot session only ever runs a
  new worker, so all its requests are stamped anyway.
- **Which turn is final.** Core judges only a pod's last result (`exit:
  true`), in this order:
  1. a human or answer entry above its `consumedSeq` exists: not final; the
     session is re-warmed when the pod ends (§6, the exit window);
  2. an open decision exists: not final, **parked**. This outranks
     `success` and `error_max_structured_output_retries` alike: a parked
     turn can exhaust the StructuredOutput retries, and its answer still
     resumes the run. A parked turn's structured output stays on its entry
     and is never returned as the result;
  3. `success`: final, `done`;
  4. any other subtype (`error_max_turns`, `error_during_execution`, …):
     final, `failed`, and its open decisions are withdrawn.
- **Core alone decides the terminal state**, in this order: `expired` (§6),
  then the rule above, then a pod that ended with no `exit` result: with an
  open decision it is parked, a non-zero exit is `AGENT_CRASHED` (ADR-0066
  §10), and a clean exit 0 with neither is `failed` with the new code
  `NO_RESULT` (§10 defines `AGENT_CRASHED` as non-zero only). On a final
  result core marks the session finished, archives it, and runs §7.

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

In a single-shot session no open decision keeps a pod beyond a bounded hold
of `DECISION_WAIT_MS` after its turn ends; a parked run holds none. ADR-0082 §5 already
made every decision non-blocking; a single-shot run adds only that its pod
does not wait for the next message after a turn ends.

- **The pod exits when its input is used up, after a grace.** When a turn
  ends on an open decision, the worker waits up to `DECISION_WAIT_MS` (the
  same knob as the live decision wait, default 45 s) for the answer before
  exiting, so a quick answer costs no new pod (owner, 2026-11-16). Today the worker runs one
  continuous `query()` and treats `success` as "wait for the next input"
  (`session.ts:1423`, `:2295-2303`). A single-shot worker instead exits
  after a `result` when nothing is left to run: the result's
  `queued_turn_count` is 0 (absent counts as not 0) and its own input queue
  is empty. An answer that arrives live after the "Pending" deny but before
  the turn ends is pushed as "Approved: retry request seq N once"
  (`session.ts:2012`) and queued by the SDK; exiting then would drop it, so
  the worker never exits while `queued_turn_count > 0`. **Consumed** means
  input that went into a completed turn: on the exiting result,
  `consumedSeq` is the highest seq of a human or answer entry the worker
  pushed, which a zero queue proves every turn has taken in. Before exiting
  it flushes anything it buffers for core (the planned worker span
  recorder's spans included), then exits explicitly (verification trap:
  `process.exitCode` is not an exit).
- **A permission parks on its stamp.** The call asks, holds for
  `DECISION_WAIT_MS`, and is denied "Pending (request seq N) … end your
  turn"; the turn ends with the stamped request open; the worker holds up to
  `DECISION_WAIT_MS` for a live answer, then writes the result and exits. Core sees the exit result with an open decision:
  parked.
- **The answer resumes it.** `RespondToPermission` (both services) and
  `ApprovePlan` warm first and append second (built, ADR-0082). An allow of a
  grantable request issues the one-time grant; the new pod replays the
  answer as "Approved: retry request seq N once"; the model retries; the
  retry's `canUseTool` spends the grant through `ConsumeGrant` and the call
  runs. A deny arrives as a message the same way. A warm that fails refuses
  the answer, and the request stays open.
- **A question parks as it does today** (ADR-0050, ADR-0082): `pending`
  after the wait, the turn ends, the pod holds up to `DECISION_WAIT_MS`, then
  exits; the answer arrives as a turn.
- **A plan.** On a fresh pod a late approval in plan mode switches mode and
  says "go ahead" (`approvePlanLate`, `session.ts:1813-1847`). If the session
  is no longer in plan mode when the answer lands, the worker can only say
  "retry once", and since `planRetry` is empty on a fresh pod
  (`session.ts:1280`, `:2009-2012`) the retried `ExitPlanMode` asks again:
  one more park. Not accepted (owner, 2026-11-16): core carries the
  approved plan, the plan decision's id and its input, to the next pod on
  the resume path, and the worker applies the approval without asking again.
- **An answer must not land on a pod that is leaving.** `WarmIfIdle` is a
  no-op while `pod_phase` reads live (`core/internal/dashboard/server.go:1175`),
  and `pod_phase` lags the exit: it is written by the provisioner's pod
  events (`ReportPodEvents`, `core/internal/coreserver/server.go:857`) and
  by the 60 s reconcile, neither at the instant the worker exits. An answer
  appended in that gap lands above the leaving pod's cursor and below the
  next pod's (`server.go:1237`, `resumeFromSeq` = `LatestSeq`): never
  delivered. So when a single-shot pod ends, core re-warms the session if a
  human or answer entry lies above the exit result's `consumedSeq`, with the
  new pod's cursor just past it, and the finality rule (§3) runs only after
  this check. This is the one change to `WarmForAppend` (the warm-then-append
  entry point since #376): an explicit cursor for this path.
- **Grant matching on retry: a per-tool match key, not the defer hook.**
  `ConsumeGrant` matches the full canonical input (`answer.go:325`), and an
  input with no canon is never grantable (`decisions.go:134-137`, `:287`).
  A resumed model re-derives the call and may rewrite free text that changes
  nothing it does, Bash's `description` above all, and the grant misses.
  The fix is a match key: `decisions` stores, and `ConsumeGrant` compares,
  the canon of a per-tool subset of the input. Known free text is dropped up
  front (owner, 2026-11-16): Bash and Agent drop `description`, WebFetch
  drops `prompt` (the URL stays in the key); every other tool keeps its
  whole input until a measured miss names a field. The card still shows the full input, and the subset never drops a
  field that changes what runs. `input_canon` is redefined in place to hold
  the match key, and every reader and writer of it switches together, in
  one PR:
  - `OnAppend`, which writes it at insert (`decisions.go:283-293`);
  - `ConsumeGrant`, which spends a grant on it (`answer.go:325`);
  - the dedup that answers a re-asked call with the open request's seq:
    `NonBlocking` (`decisions.go:120-128`), called by `SendMessage`
    (`core/internal/coreserver/server.go:206-211`), and `OpenRequest`
    (`answer.go:251-260`).
  With the dedup on the full input and the grant on the key, a retry that
  differs only in `description` would open a second card for one call, and
  an allow on one would beat a deny on the other through `ORDER BY
  request_seq`. Stored rows keep their full canon: the key is computed in
  Go, so no migration can rewrite them. A retry of a pre-change request with
  a `description` therefore misses both the dedup and its grant, and asks
  once more as a new decision: one extra card per open request at deploy
  time, in the safe direction, and none once those are answered.
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
  and a retry that misses its grant parks again. A park is a pod exit that
  leaves an open decision, not an ask (a turn may ask and get its answer
  live). A single-shot run that parks more than `limits.maxParks` times
  (default 10, set per definition in its reviewed file) ends `failed`
  (`PARK_LIMIT`). The count needs no column: it is the session's `exit:
  true` results at whose `created_at` some decision of the session was open
  (its `created_at` earlier, its `answered_at` null or later; both columns
  are in migration 000023). Core reads it when it judges an exit result
  parked, a handful of rows per run. If it ever proves slow, a `parks`
  counter on `sessions` is the upgrade, as a new migration pair.
- **A pod exits mid-call.** The idle sweep, a crash or the lifetime cap can
  end a pod while a tool runs or a call waits in `canUseTool`. The CLI's
  JSONL then holds a `tool_use` with no `tool_result`. ADR-0050 found the
  resume behaviour undocumented; the bundled CLI has code that finds an
  unresolved `tool_use` on resume, but nothing here relies on it. The worker's
  `released`, `spentDecisions` and `planRetry` are memory only
  (`session.ts:1268-1280`), so the next pod has none: a late approval of a
  call the dead pod held becomes "retry once" (grant still spendable, once),
  a plan approval is carried by core to the next pod (above), so it does
  not ask again. The decision row is durable, so
  nothing is lost that a person gave. **Required test** (the worker-exit
  slice): resume a session whose JSONL ends on an unanswered `tool_use`,
  check that the SDK starts, that the next turn runs, and that an open
  decision for that call is still answerable and its grant spendable.
- **A parked session takes only its answer.** `WarmIfIdle` and the message
  paths (the console's Warm, `PostMessage`, a dispatcher wake) refuse a
  single-shot session with an open decision; only its answer warms it.
- **Resident threads** are unchanged: their pod stays for the next message,
  and a decision there is ADR-0082's as built.
- **Expiry.** `limits.answerTimeout` (a duration; single-shot sessions only;
  default 24h, strictly less than `SESSION_RETENTION`, and the retention
  sweep skips parked sessions) bounds how long a decision may wait. Past
  it, core ends the session `expired` (the decisions go `expired`, a state
  migration 000023 already allows) and runs §7. This amends ADR-0082 §5,
  which gives `expires_at` to cards only: in a single-shot session a
  question, permission or plan expires too, at `limits.answerTimeout`; the check rides
  `sessions.Loop`. A parked session whose definition changed since its pin
  cannot warm, so core ends it `failed`. Nothing is answered for anyone.
- **No migration.** Parked is derived, as every state is (ADR-0048 §2):
  `lifecycle = 'single-shot'`, no live pod, an open row in `decisions`
  (migration 000023 has `state`, `created_at`, `answered_at` and `UNIQUE
  (session_id, request_seq)`, which is all §6 reads, the re-park count
  included). A slice that finds it needs a column adds a new numbered pair.

### Owner answers (2026-11-16)

1. **Re-park cap:** default 10, a definition limit (`limits.maxParks`).
2. **Grant match key:** drop known free text now: Bash and Agent
   `description`, WebFetch `prompt`.
3. **Exit timing:** a grace of `DECISION_WAIT_MS` after a turn that ends on
   an open decision, then exit.
4. **Exit-window re-warm:** yes, with an explicit cursor on the warm
   (`WarmForAppend`, which #376 made the one warm-then-append entry point).
5. **Plan approval on a fresh pod:** core carries the approved plan to the
   next pod; no second ask.
6. **Exit code:** a clean exit 0 with no result and no open decision is
   `failed` / `NO_RESULT`; `AGENT_CRASHED` stays non-zero only.

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
   ≤ 64 KiB); `agentSpec` fills `lifecycle` from the session row for
   `single-shot` only. Entry: `GetSession` for any definition session
   (resident reads `""`). Test: loader table tests; `agent_spec_test.go`
   for both lifecycles, and that a resident session's `agent_spec_sha` is
   byte-identical to the one main computes today (so no live pod is refused
   after the deploy). First, because every later worker slice reads it.
2. **The result entry and its key.** `POST /message` takes
   `idempotencyKey`; the worker writes `result_text` and
   `structured_output` on every `result`, keyed
   `result:<session>:<consumedSeq>:<k>`.
   Entry: every session's result (resident too). Test: sidecar handler
   test that the key reaches core; an integration test that two results
   with distinct keys are two rows and a retried one is one.
3. **The grant match key.** `input_canon` holds the per-tool subset's canon,
   at all three call sites at once (`OnAppend`, `ConsumeGrant`, the
   `NonBlocking`/`OpenRequest` dedup). Entry: today's resident sessions'
   grants. Test: a Bash retry with a new `description` gets the open
   request's seq back (no second card) and spends the grant; one with a new
   `command` does neither. Agent `description` and WebFetch `prompt` are
   dropped too: a WebFetch retry with a new `prompt` spends the grant, one
   with a new URL does not. Before parking, since parking makes retries
   routine.
4. **`interaction`, release one.** Loader optional and ignored, the request
   hash's constant, `AskUserQuestion` in every definition's `sidecarTools`,
   the worker's deny branch gone. A valid `(name, sha)` row's `effective` is
   frozen (`core/internal/definitions/registry.go:175-178`), so the loader's
   addition reaches only rows synced at a new sha: a live pod keeps its
   pinned spec and its `agent_spec_sha`, and is not refused. A definition
   whose effective list lacks the tool gets it by a file edit, which moves
   its pin the usual way (its next warm starts a new thread, ADR-0066 §6).
   Entry: the sync. Test: loader and hash tests; an old claim still
   matches; a frozen row's sha is unchanged. Before `Invoke`, so `Invoke` never
   reads it.
5. **`Invoke` and `WaitForResult`, with the final-turn rule.** Handlers,
   proto additions (`pending`, `thread_id` on `CAPACITY`, `waiting`,
   `expired`), §3's finality. The worker still runs continuously and
   writes no `exit` result yet, so until slice 7 core applies rules 2 to 4
   to every result.
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
7. **The worker exits when its input is used up**, flushing first, with
   `consumedSeq` and `exit: true` on its last result; core judges only exit
   results, re-warms for an answer above `consumedSeq` before the finality
   rule (an explicit cursor on `WarmForAppend`), and labels a clean exit
   with no result `NO_RESULT`. A turn that ends on an open decision first
   holds up to `DECISION_WAIT_MS` for a live answer. Core carries an
   approved plan (decision id and input) to the next pod, which applies it
   without a second ask. Entry:
   `Invoke`. Test: worker tests that a single-shot `success` with
   `queued_turn_count` 0 exits, one with a queued live answer runs that
   turn first, and a resident one never exits; integration test of an
   answer appended in the exit window reaching the next pod; the mid-call
   resume test (§6); an answer inside the hold needs no new pod; a late
   plan approval on a fresh pod does not re-ask.
8. **§7 teardown and sweep** on final result, crash and `Stop`. Entry:
   `Invoke`. Test: fake provisioner records `TearDownSession` then
   `SweepSession` per path; a parked run keeps its PVC.
9. **Parked is respected.** `WarmIfIdle` and the message paths refuse a
   parked single-shot session except by its answer; `Status`/
   `WaitForResult` read `waiting` without a pod; the re-park cap, with the
   `limits.maxParks` spec field (loader: positive int, default 10). Entry: `Invoke`, then an answer. Test: Warm and
   `PostMessage` refused; the answer warms; the eleventh park ends `failed`.
10. **`maxParked` / `PARKED_LIMIT`.** Client file field, `Invoke` refusal.
    Entry: `Invoke`. Test: the twenty-first parked run is refused, not
    queued.
11. **`answerTimeout` and `expired`; a changed definition ends `failed`.**
    Both ride `sessions.Loop` and end through §7; the retention sweep
    skips parked runs in the same PR, since `answerTimeout` is what bounds
    them once it does. Entry: a parked run left alone. Test: a past-timeout run reads `expired` with its PVC gone; a
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
- A single-shot run's pod lives until its input is used up, then exits,
  after a bounded hold of `DECISION_WAIT_MS` when the turn ended on an open
  decision.
  Every turn writes a result with structured output; only core's check,
  after the exit-window re-warm, makes one final.
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
    permissions. Check that each pod exits once its turn ends and `DECISION_WAIT_MS` has
    passed, that each
    run reads `waiting`, and that answering all of them finishes each one
    `done`.
  - Answer a permission whose retry rewrites Bash's `description`: it runs
    on the grant without a second ask.
  - Leave one past `answerTimeout`: it reads `expired`, with its PVC gone.
  - Answer a permission while the fleet is full: the answer is refused,
    and the request stays open.
