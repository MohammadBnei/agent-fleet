# ADR-0050: A blocking question outlives its pod; its answer is delivered on warm

**Status:** Accepted
**Date:** 2026-08-15

## Context

`docs/adr/0018` made `AskUserQuestion` a real MCP tool: it appends a
`type: "question"` transcript row and long-polls (default 60s, in `core`) for
a matching `answer` row, returning `{"status":"pending"}` on timeout so no
single HTTP call blocks indefinitely.

That design assumed the pod that asked is the pod that receives — the poll
runs in `core` on behalf of a *live* worker whose tool call is mid-flight.
Two facts break that assumption for any question a human takes real time to
answer:

1. **A human can take days.** A question that shapes the task ("which of these
   four schemas?") is exactly the kind a human researches before answering.
   60s, or any bounded poll, times out long before.

2. **The pod is torn down while waiting.** `enforceIdleTimeout` (default
   30min, keyed on `last_active_at`) reaps an idle pod. Once the pod is gone,
   the mid-flight `AskUserQuestion` tool call is gone with it — an interrupted
   MCP tool call is a *paused turn waiting on a `tool_result`*, and the SDK
   does **not** re-issue it on resume (verified: dangling `tool_use` behavior
   on resume is undocumented, so it cannot be relied on). The turn never
   completes; the answer, when it lands, reaches nobody.

So on `{"status":"pending"}` the agent had no mechanical way to ever receive
the answer after teardown. It would drop the thread and wander off — the
reported symptom.

A first design routed blocking questions through the **same machinery as
`canUseTool`** (a parked promise in the worker, re-driven by the SDK on warm).
A `doubt-driven-development` review proved this fatal: permission and
`AskUserQuestion` live in different processes with different resume stories,
and `ReserveSlot` auto-answers unanswered questions on every warm. The parked
promise is disposable and re-drivable *only because the SDK re-invokes
`canUseTool`*; nothing re-invokes an MCP tool call. The two are not the same
machinery.

The constraint from the outset: the fix must be **mechanical**, not a prompt
telling the model to re-invoke in a loop — that burns tokens polling for
something that should arrive on its own.

## Decision

**A question does not block across teardown. It returns `pending`, the turn
ends, the pod is free to die, and the answer is delivered to the next pod on
warm** — reusing the transcript-replay path that already exists for every
human message.

Three mechanical changes, no proto change, no new store:

1. **The `answer` row is delivered as an ordinary input turn on warm.**
   `worker/src/session.ts` used to *drop* `answer` entries from the human
   message stream (`if (entry.type === "answer") return`) on the assumption
   they were always consumed by a live poll. Now, if no live poll consumed it,
   the same `streamHumanMessages` replay (from `RESUME_FROM_SEQ`) that
   redelivers every human message on warm pushes the answer in as a labeled
   turn: `Answer to your earlier question (seq N): <payload>`. This is the
   only path that delivers a choice made after teardown.

2. **`ReserveSlot` no longer auto-answers questions.** Its stale-close (which
   denies decisions the dead pod was waiting on) now closes **only**
   `permission_request` rows. A permission's allow/deny buttons are bound to a
   specific live pod's `canUseTool` promise — once that pod is gone the buttons
   are dead and the row *must* be closed. A question is not: its answer is a
   durable row keyed by the question's own seq, deliverable to any later pod.
   Stale-closing it would auto-answer it "restarted before answered" the
   instant the pod warms — destroying the exact answer the human was about to
   give.

3. **The tool tells the model to end its turn, not re-invoke.** The
   `AskUserQuestion` description and its `pending` payload
   (`sidecar/internal/mcpserver/server.go`) now say: the question is live and
   durable, do **not** call again, end your turn; the answer arrives as a
   normal message when the human replies, even days later across a restart.

## Consequences

- **A question card stays live on the dashboard indefinitely.** It is counted
  in `PendingDecisions` (`type IN ('question','permission_request')` with no
  reply) until answered, and survives every warm. This is the intended
  behavior, not a leak — the human answers on the same card whenever they are
  ready.

- **Double-delivery is possible and benign.** If a live poll *is* mid-flight
  when the answer lands, the agent gets it both ways: as the tool's
  `tool_result` and, on a subsequent warm, as an input turn. The redundant
  turn restates a choice the agent already acted on — harmless.
  `ponytail:` dedup by `replyTo` if it ever measurably bites.

- **Subsidiary vs blocking is a model decision, not a wire flag.** No
  `blocking` field was added. `reuseOrAppendQuestion` (re-joining an
  unanswered question by identical text) plus the fast `pending` return
  already give the model both options: treat `pending` as "answer arrives
  later, continue" (subsidiary) or end the turn and wait (blocking). Add a
  `blocking` flag only if a question must *force* a stop rather than let the
  agent default and proceed.

- **Guarded.** `TestReserveSlot_ResolvesDecisionsTheDeadPodLeftBehind`
  (`-tags=integration`) now asserts a question survives warm
  (`PendingDecisions == 1`) and collects no auto-reply, while the stranded
  permission is still closed.

- **Supersedes `docs/adr/0018`'s "one call outstanding, next answer matches"
  poll model only for the teardown case** — the in-pod live poll is unchanged
  and still the fast path when the human answers within the window. Everything
  else in `docs/adr/0018` holds.
