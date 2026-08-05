# ADR-0024: Pod-crash fast-path detection, a real retry cap, and a `knowledge_journal` read path

**Status:** Accepted
**Date:** 2026-08-05

## Context

A reliability audit (`docs/reliability-findings.md` finding #1) found
worker-pod failure handling fragmented across three mechanisms that never
actually coordinated with each other:

- `provisioner/internal/reconcile/loop.go`'s reconcile poll GC'd terminal
  pods but told `core` nothing about *why* a pod ended.
- `core`'s `SetTaskStatus` only tore a session down opportunistically —
  it fires only if the worker pod itself cooperates by writing a terminal
  status first. A pod that crashes hard (OOM, node eviction, a panic
  before it can call `SetTaskStatus`) never triggers this path at all.
- `core/internal/tasks/store.go`'s `ClaimNextTask` heartbeat reclaim was
  the *only* mechanism that didn't depend on the crashed pod's own
  cooperation — but it's blind for up to 10 minutes by design (the
  staleness window), since nothing else was watching a scheduled pod's
  live status in between.

Separately: `retry_count` was tracked on every reclaim but never capped —
a task whose worker pod kept crashing for the same underlying reason
(a bad commit, a broken dependency) would loop through reclaim
indefinitely instead of ever giving up. And `knowledge_journal` already
received pod-lifecycle events (`ReportPodEvents`) and `AppendJournal`
calls, but had no read RPC anywhere — even a successfully journaled crash
was invisible without direct Postgres access, which no component outside
`core` is allowed to hold (ADR-0020 point 1).

## Decision

1. **The reconcile loop reports a crash event to `core` immediately, as a
   fast-path accelerant on top of — not a replacement for — the
   heartbeat-reclaim fallback.** ADR-0022's new `EventReporter` wiring:
   on detecting a worker `Job` that reached `Failed` phase, the loop
   calls `ReportEvent(..., Phase: POD_PHASE_CRASHED, ...)` before GC'ing
   the Job. `core`'s `ReportPodEvents` handler, on seeing a
   `POD_PHASE_CRASHED` event for a worker session, calls a new
   `tasks.Store.MarkCrashed(ctx, taskID)` — which backdates the task's
   `heartbeat_at` to just past the staleness window, so the *next*
   `ClaimNextTask` dispatch tick (seconds away, not minutes) treats it as
   immediately reclaim-eligible. This reuses the existing reclaim
   mechanism rather than inventing a second one, and keeps Postgres as
   the sole durable ground truth for task state (ADR-0020 point 3) — a
   missed or dropped push event just degrades gracefully back to the
   original 10-minute heartbeat path, it doesn't leave the task stuck.
   `MarkCrashed` is a no-op (not an error) for a task that already
   reached a terminal status — a race between this push and `core`'s own
   opportunistic teardown is expected and harmless.
2. **`ClaimNextTask` gains a `maxRetries` parameter
   (`MAX_TASK_RETRIES`, default 3).** A reclaim that would push
   `retry_count` to the cap sets the task to a new terminal status,
   `failed_permanently`, instead of reclaiming it again — the method
   returns no claimable task for that row in that case, rather than
   handing back a task for the dispatch loop to spawn yet another pod
   for.
3. **A new, typed `GetJournal` RPC on `DashboardService`**, backed by a
   new `journal.Store.List(ctx, repo, sinceID, limit)` — the same
   pull/cursor shape `transcript.Store.ReadSince` already uses. Typed
   request/response messages, explicitly not a generic `Query(bytes)
   returns (bytes)` dispatcher: two concrete read gaps don't justify
   giving up protobuf's type safety for a fully general one.

## Alternatives considered

The real fork on the crash-detection side was "poll faster" vs. "push on
an event that already existed." The push wire (`ReportPodEvents`) already
existed — this decision is mostly about actually consuming it for
dispatch-relevant state, not building new plumbing. On the journal-read
side, the only seriously named and rejected alternative was the generic
byte-dispatcher above — narrower, typed RPCs were preferred once it was
clear only two call sites needed a read path at all.

## Consequences

- Crash-to-reclaim latency drops from "up to 10 minutes" to "about one
  dispatch tick" for the common case (a `Job` reaching `Failed`), without
  weakening the heartbeat fallback's role as the thing that still catches
  everything else (a hung pod that never reaches a terminal k8s phase at
  all, a dropped `ReportEvent` call, `core` itself restarting mid-event).
- A task can now reach `failed_permanently` — a new value in
  `tasks_status_check` — meaning "don't dispatch a pod for this again."
  Distinguishing it from `failed` (a single attempt's outcome, still
  eligible for reclaim) matters for anything building on top of task
  status later.
- Crash visibility surfaces in the dashboard only, via `GetJournal` — this
  was a deliberate choice not to expand Discord's scope to cover it;
  Discord stays the narrative/approval channel, the dashboard is where
  operational/debugging state lives.
