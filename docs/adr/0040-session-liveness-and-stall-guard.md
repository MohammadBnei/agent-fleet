# ADR-0040 — Session liveness is a derived second dimension, and a silent pod is torn down in minutes

**Status:** Accepted
**Date:** 2026-08-12
**Supersedes:** nothing. **Related:** `0020` (core commands, provisioner
executes), `0029` (a session is the durable unit), `0030` (migrations are
the schema's sole source of truth), `reliability-findings.md` §1.

## Context

Two gaps, one shape: the fleet could not answer "is this session actually
alive, and if not, whose turn is it?"

**1. Nothing bounded "pod scheduled" → "first sign of life."** `core`
commands the provisioner and returns
(`core/internal/dispatch/loop.go`) — `CreateWorkerPod` replies as soon as
the Job exists, and nothing watches for the worker to say anything. The
only backstop was `enforceIdleTimeout`, whose clock starts at claim time:
`ClaimNextTask` sets `last_active_at = now()`, so a pod that comes up and
never speaks looks *freshly active* for the full 30-minute idle timeout,
then needs a further 10 minutes of heartbeat staleness before
`ClaimNextTask` will reclaim it. About **40 minutes** of a dead pod holding
one of `MAX_IN_FLIGHT_TASKS` slots. `reliability-findings.md` §1 describes
the same invisibility window from the crash side; this is the narrower
never-started case.

**2. "Which session needs me?" had no server-side answer.** `tasks.status`
is a workflow status (`proposed`/`pending`/`claimed`/`running`/…) and says
nothing about liveness — a session can be `running` and blocked, or
`running` and stalled. The dashboard papered over this with `isCogitating`,
a client-side heuristic that walked the transcript backwards guessing
whether a turn was in flight. It existed only on desktop, so mobile showed
nothing, and every future client would have had to reimplement it.

[herdr](https://github.com/herdrdev/herdr) solves the same problem for
terminal panes: every pane is marked working, blocked or idle, so nobody
hunts for the one that stopped, and `agent prompt --wait` fails with
`agent_prompt_stalled` if a prompt produces no state change rather than
hanging forever. Both ideas port; the terminal-scraping machinery
underneath them does not, since the Agent SDK already gives us structured
events.

## Decision

### Liveness is derived, not stored

`live_state ∈ {"", working, blocked, idle, done, stalled, unknown}`, computed
by `tasks.DeriveLiveState` — a pure function of one `tasks` row, evaluated
on every read (`TaskToProto`).

**Rejected: a stored `live_state` column.** It would need a write path on
every transition, including ones with no write to hang off (a turn becomes
`stalled` because time passed, not because anything happened). A cached
enum that nothing updates on a clock is a value that is wrong exactly when
it matters. Deriving cannot drift, and the inputs are already maintained.

The migration therefore adds **inputs**, not a state:

| Column | Why it can't be answered without it |
|---|---|
| `last_entry_type`, `last_entry_from` | Distinguishes "the agent closed the turn" (a `result`) from "a human said something and nothing came back". Written by the same `activityTrackingStore` UPDATE that already sets `last_active_at` — no extra round trip, no second source of truth. |
| `activity_seen` | Latches on the first agent-authored entry, reset when a new pod is dispatched. `last_active_at` cannot express this: claiming sets it to `now()`, so a never-started pod looks freshly active. |
| `seen_at` | Separates `idle` from `done`. |

`status` is untouched. Liveness is orthogonal, and collapsing them is what
made "is it stuck?" unanswerable in the first place.

### `done` means "finished while you weren't looking"

herdr's distinction, and the reason it earns a state of its own: in a fleet
you are not watching continuously, work that completed unattended is the
thing you actually want surfaced. `MarkSeen` is a dedicated RPC fired when
a human **opens** a session — deliberately not a side effect of `GetTask`,
because the task list polls, and a poll marking everything seen makes
`done` unreachable.

### Two stall clocks, only one of which acts

- **Startup stall** (`STARTUP_STALL_MS`, default 3 min) — pod live,
  `activity_seen` false, past the threshold → tear down, record
  `last_error`, let the existing heartbeat-reclaim path retry within
  `MAX_TASK_RETRIES`. Runs in the existing 30 s sweep tick next to
  `enforceStopGrace`/`enforceIdleTimeout`, and like both of them changes no
  status: the worktree and saved `session_id` survive, so the session stays
  resumable.
- **Turn stall** (`TURN_STALL_MS`, default 90 s) — the agent owes a
  response and hasn't produced one. **Purely informational; nothing is torn
  down.** A slow turn is not a dead one, and the human can interrupt if
  they disagree. This is the one deliberate divergence from herdr, whose
  `agent_prompt_stalled` is an error return — it can afford that because a
  human is watching the pane it fires in.

A long tool call cannot trigger either: an agent mid-work keeps appending
entries, so `last_active_at` moves on its own, and `stalled` additionally
requires that the *last* entry be human-originated.

## Consequences

- A silent pod is reclaimed in ~3 minutes instead of ~40, and says why
  (`last_error`) instead of vanishing.
- `isCogitating` is deleted. Both clients read one server-derived value, so
  they agree on what "working" means by construction, and mobile gets a
  state it never had.
- `DefaultTurnStall` is duplicated as a constant in `dashboard/server.go`
  rather than threaded through every `TaskToProto` call site (including
  `coreserver`'s). Diverging from `TURN_STALL_MS` only changes how quickly
  the UI says `stalled`; nothing acts on it. If a third consumer appears,
  thread the config properly.
- Pre-existing rows are backfilled from the transcript in the migration —
  without it every currently-warm session would look like a pod that never
  spoke and be torn down on the first sweep after deploy.
- Not addressed: **identity re-check.** `worker/src/session.ts` still
  overwrites `tasks.session_id` with whatever the SDK returns without
  comparing it to `RESUME_SESSION_ID`, so a silently-failed resume is
  indistinguishable from a successful one, and every session-control RPC is
  keyed by `task_id` alone with no lease check. Deliberately out of scope
  here; it becomes materially more important if agents can write into each
  other's sessions.
