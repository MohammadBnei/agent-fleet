# ADR-0029: Sessions replace tasks as the durable unit; canUseTool becomes a live permission prompt, not a prospective approval gate

**Status:** Accepted
**Date:** 2026-08-07

## Context

ADR-0021/ADR-0025 built a real, working coordination model: one continuous
streaming-input session per task, `Write`/`Edit` structurally absent from
`allowedTools` for the whole session, `canUseTool` denying them (plus a
`MUTATING_BASH_RE` heuristic for Bash) until a structured `/approve` signal
flips an in-memory flag and calls `q.setPermissionMode("default")`. It
worked, but it was architecturally rigid in a specific way: every task got
exactly one binary planning→implementation transition, enforced by the
fleet on top of the SDK, not by the SDK's own permission system. Talking to
the agent meant posting into a Discord thread (or the dashboard's mirror of
it) and waiting for the relay loop — nothing like the immediacy of running
`claude` locally.

An architecture interview (see the interview transcript preserved in this
repo's own session history, not reproduced here) surfaced the actual
complaint: not that async/durable coordination was the wrong choice
(ADR-0013's reasoning for a pull/cursor Postgres table over a bare
streaming watch still holds — a dropped message during a live decision
isn't recoverable), but that the fleet had built a second, fleet-specific
permission model *on top of* the Agent SDK's own, when the SDK's real
`permissionMode` type (`'default' | 'acceptEdits' | 'bypassPermissions' |
'plan' | 'delegate' | 'dontAsk'`, confirmed directly against
`coreTypes.d.ts`, not assumed) already does exactly this, mode-switchable,
with ADR-0027's `SetPermissionMode` transport already built and already
working end-to-end.

Two further findings drove scope beyond a permission-model swap alone:

1. Tracing the bundled SDK's own `cli.js` (not guessed) showed session
   transcripts save to `$CLAUDE_CONFIG_DIR/projects/<cwd, non-alnum
   replaced with '-'>/<sessionId>.jsonl` — no hash, a direct cwd encoding.
   `CLAUDE_CONFIG_DIR` was never actually set anywhere in this fleet
   despite ADR-0016 describing the redirect; `resume:` therefore had
   nothing to resume from, ever, regardless of what session id was passed.
2. A pod tied to a task's entire lifetime doesn't fit "come back to a
   session hours later and keep talking to it" — the pod needed to become
   ephemeral compute attached to a session on demand, not a fixed
   accompaniment to the task row.

## Decision

1. **`canUseTool` reproduces the SDK's own CLI-parity permission tiers
   instead of a second, fleet-specific gate.** It does zero tool
   classification of its own — `MUTATING_BASH_RE` is deleted outright, not
   widened. The SDK's real `permissionMode` already decides *when*
   `canUseTool` gets invoked (`bypassPermissions` skips it entirely,
   `acceptEdits` skips it for file edits, `plan` blocks mutation without
   invoking it, `default` invokes it for anything not auto-safe);
   `canUseTool`'s only job is "ask a human and block," reusing
   `ExitPlanMode`'s pre-existing ask-and-block-and-resolve-on-later-event
   pattern (`worker/src/session.ts`, formerly `planning.ts`), generalized
   from one hardcoded case to a `Map<seq, resolver>` so parallel tool
   calls in one assistant turn each get their own correlated prompt.
   `TranscriptEntry.reply_to` is now wire-visible (the Go struct already
   carried it; the proto message didn't), which is what makes this
   generalization sound instead of adjacency-inferred.
2. **Sessions start in `"default"` mode** — matches running `claude`
   locally with no flags — **not a forced `"plan"` gate.** `plan` is still
   fully available, just as one selectable mode via the dashboard's mode
   picker (now showing `default`/`plan`/`acceptEdits`/`bypassPermissions`
   with the real active mode highlighted, sourced from a new
   `tasks.permission_mode` column) instead of a mandatory starting state.
3. **`Approve` is deleted end-to-end** — proto RPC, Go handler, Discord
   `/approve` command and handler (plus explicit stale-command pruning:
   `ApplicationCommandCreate` only upserts by name, it never deregisters a
   command dropped from the list). There is no plan→default flip left to
   fix a button to; `SetPermissionMode` (widened to accept `default`/`plan`
   alongside the pre-existing `acceptEdits`/`bypassPermissions`) is the
   only mode lever, and a new `RespondToPermission` RPC (`AnswerQuestion`'s
   sibling, same `AppendReply`-by-seq shape) answers individual
   `canUseTool` prompts.
4. **Sessions, not tasks, are the durable unit.** A pod is ephemeral
   compute attached to a session on demand — a `Warm` RPC boots one for an
   idle session (threading a saved `session_id` through as `resume:`),
   `Stop`/an idle-timeout backstop tear it down, neither ever touching
   `tasks.status` (a loose UI-freshness signal now, not control flow — the
   worktree and saved session id both survive either kind of teardown, so
   the session stays resumable). Pod-lifecycle gating (the concurrency
   cap, the stop-grace force-teardown sweep, the idle-timeout sweep) all
   moved from `status`-based checks to `pod_phase`-based ones, since status
   no longer reliably tracks "is there a live pod."
5. **`CLAUDE_CONFIG_DIR=/workspace/.claude-home`** is now actually set on
   the worker container (ADR-0016's described-but-never-wired redirect),
   and `resume_session_id` threads end-to-end from `tasks.session_id`
   through `CreateWorkerPodRequest` to the worker's `resume:` option. One
   shared root is safe across concurrent tasks because the SDK's own
   per-project directory naming is a direct `cwd` encoding and every
   task's `cwd` is already a distinct worktree path — confirmed against
   the SDK source, not assumed.
6. **The idle-timeout backstop's activity signal is real transcript
   activity**, not the sidecar's telemetry (silent whenever there's no
   uncommitted git diff — most of a normal conversation) or `heartbeat_at`
   (fires unconditionally on a timer, proves the pod is alive, not that
   anything is happening). A `transcript.Store` decorator constructed once
   in `cmd/core` touches `tasks.last_active_at` on every real
   `Append`/`AppendReply`, deliberately untyped by entry type — during
   active autonomous work the worker relays every SDK message as its own
   entry, so a genuinely busy session keeps touching this on its own.
7. **The "planning" framing is renamed throughout, not just conceptually
   retired** — `planning_transcript` → `transcript` (a guarded rename,
   verified against real pre-existing data, not just a fresh install),
   `tasks.planning_session_id` → `session_id`, the `"planner"` transcript
   actor → `"agent"`, `worker/src/planning.ts` → `session.ts`, and task
   status `planning`/`implementing` → `running` (a clean cutover with a
   one-time data migration — homelab scale, no rolling-replica window
   where an old and new binary would both be writing the column at once,
   so no tolerated-legacy-value period was needed).

## Alternatives considered

- **Keep the phase-boundary/approve-signal model, just make relay faster
  (websockets instead of poll)** — rejected: the complaint was the
  phase-gating concept itself, not its latency.
- **A real terminal/tty attach (`kubectl exec` into a pod running an
  interactive `claude`)** — rejected in favor of the dashboard reusing its
  already-built streaming-transcript infrastructure into a real chat +
  live permission-prompt UI; the dashboard is the surface already being
  invested in.
- **A fleet-specific one-shot-approval-then-free-for-all permission
  model** ("approve once, allow everything after") — rejected in favor of
  exact parity with the SDK's own tiers, since matching CLI ergonomics was
  the explicit goal.
- **Tolerate `planning`/`implementing` as legacy-but-valid status values
  for one release** (the pattern used elsewhere in this schema for
  additive changes) — rejected for this specific rename: homelab-scale,
  single deploy, no window where two binary versions write the column
  concurrently, so a clean cutover with a one-time `UPDATE` is simpler and
  loses nothing.
- **Auto-warm a message to a still-`pending` (never-yet-claimed) task** —
  rejected: `dispatch.Loop`'s `ClaimNextTask` already owns a fresh task's
  first pod: warming it too would double-dispatch, since neither path
  knows about the other's in-flight attempt. `Warm`'s handler rejects a
  pending task explicitly (a human clicking it deserves to know why);
  `Discuss`'s shared `warmIfIdle` helper silently skips warming for that
  same case instead, since it has a message to append regardless.

## Consequences

- A meaningfully smaller worker permission surface: no
  `MUTATING_BASH_RE`, no `approved` boolean, no fleet-specific
  classification to keep in sync with what the SDK actually does.
- The dashboard is now the primary live interaction surface in practice,
  not just in framing — a `PermissionCard` (generic) alongside the
  existing `PlanCard` (ExitPlanMode-specific), both submitting through
  `RespondToPermission`.
- Discord is demoted to a secondary/notification channel — `/task`,
  `/stop`, and the pending-question-reply special-case survive
  unchanged; `permission_request`/`permission_response` are deliberately
  never added to `discordSafeTypes` (the same poor-fit reasoning
  ADR-0018 already gave for `AskUserQuestion`).
- `tasks` keeps its name and most of its shape (repo/description/pr_url/
  etc. are all still task-scoped, not session-scoped) — this is a
  reframing of what the row's lifecycle *means*, not a schema split. A
  future pass could rename the table itself if the task/session
  distinction ever needs to be literal, not just conceptual; not done
  here since nothing currently needs two separate rows per unit of work.
- `e2e_sessions` remains dead code in the schema (per ADR-0020 point 1,
  unrelated to this change) — still not addressed.

## Superseded

- **ADR-0021** — the streaming-input-mode-per-task decision and its
  `setPermissionMode()`-on-approval mechanics are superseded by this
  ADR's points 1–3; ADR-0021's point 6 (`resume: sessionId` kept only for
  crash recovery, disk-based reload) is superseded by this ADR's point 5,
  which makes `resume:` a real, working, general-purpose mechanism.
- **ADR-0025** — point 1–2 (no round cap, no phase boundary, agent paces
  itself) is unchanged and still holds; point 3 (the approve-signal
  mechanism, `/approve` as the sole structured trigger) is superseded by
  this ADR's point 3. Points 4 (question-seq correlation) and 5 (allowlist
  relay) are unchanged and still hold — this ADR's `reply_to` wire fix
  extends point 4's same correlation mechanism to permission requests, it
  doesn't replace it.

## Not superseded (still holds, noted for cross-reference)

- **ADR-0016** — the crash-recovery/reclaim mechanics are unchanged; its
  `CLAUDE_CONFIG_DIR` redirect, described but never implemented, is what
  this ADR's point 5 actually builds.
- **ADR-0017** — single-session planning, no proposer/critic split;
  unaffected by the phase-boundary's removal since ADR-0017 never
  required one.
- **ADR-0018** — `AskUserQuestion`'s ask-and-long-poll pattern is reused
  verbatim as the template for the permission-prompt mechanism, not
  replaced.
- **ADR-0020** — hub-and-spoke, MCP-local-only, gRPC-only-inter-pod, and
  point 2's "provisioner never decides pod lifecycle on its own" are all
  unchanged; this ADR's idle-timeout sweep living in `core`'s dispatch
  loop rather than the provisioner is a direct application of point 2,
  not a new decision.
- **ADR-0027** — `SetPermissionMode`'s transport (transcript entry →
  sidecar SSE → `q.setPermissionMode()`) is reused as-is, not rebuilt;
  only its allowlist (now including `default`/`plan`) and what calls it
  (the new mode picker, replacing the old binary Approve) changed.
