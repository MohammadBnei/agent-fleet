# ADR-0016: Task crash recovery, heartbeat reclaim, and transient-error retry

**Status:** Superseded by [0048](0048-one-session-one-pod-one-shared-home.md) — the lease/heartbeat/retry machine is replaced by reconciling `pod_phase` against Kubernetes every 60s
**Date:** 2026-08-04

## Context

A live incident: `dream-analyst-worker` task `ca6cbbfb-1945-4879-97e6-a8d60ba9a56b`
died with `error_during_execution after 0 turns, $0` — a transient SDK/infra
hiccup, not a genuine implementation failure — and there was no way to recover
it. `worker/src/index.ts`'s single top-level `catch` treated it identically to a
real failure: the task was marked `failed` and the work was gone.

This was already a known, accepted gap: `docs/adr/0003`'s Consequences section
states "a worker pod crash mid-task leaves that task `claimed`/`planning` in
Postgres with no automatic requeue — currently a manual fix, not yet automated."
This ADR closes it.

A second, initially-missed piece: Claude session state is not purely
server-side/opaque. Reading the installed Agent SDK's own bundled source
(`node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs:7010`) confirms it resolves
`~/.claude` via `process.env.CLAUDE_CONFIG_DIR ?? join(homedir(), ".claude")`,
and session transcripts live under `~/.claude/projects/<hash>/<sessionId>.jsonl`.
Without redirecting `CLAUDE_CONFIG_DIR` onto durable storage, persisting a
session id to Postgres alone would have been a dead end — the local transcript
file `resume` depends on dies with the container on every pod restart.

## Decision

**1. Claude's own data moves onto the existing worker PVC.** Both
`k8s/dream-analyst-worker.yaml` and `k8s/vos-monolith-worker.yaml` set
`CLAUDE_CONFIG_DIR=/workspace/.claude-home` — a sibling directory to the
already-PVC-backed `worktrees/` and `repo/`. No new PVC, one env var per worker.

**2. Postgres persists the session-id *pointer* + orchestration state.** New
`tasks` columns: `proposer_session_id`, `critic_session_id`, `retry_count`,
`last_error`, `heartbeat_at`, `lease_id`. New status value `implementing`,
distinct from `planning` (both previously shared `status='planning'`), so a
reclaimed task can tell which phase it died in.

**3. `claimNextTask` also reclaims stale in-flight rows.** Its `WHERE` clause now
matches `status='pending'` OR (`status IN ('claimed','planning','implementing')`
AND heartbeat stale for 10+ minutes, treating `heartbeat_at IS NULL` as stale so
pre-migration rows aren't permanently stranded). Critically, the `UPDATE`
**preserves** `status` on reclaim (`CASE WHEN status = 'pending' THEN 'claimed'
ELSE status END`) instead of overwriting it — an earlier draft of this design
got this wrong, which would have destroyed the exact signal ("which phase did it
die in?") the reclaim needs. A fresh `pending`→`claimed` transition still fires
normally.

**4. Only implementation-phase resume is real; planning always restarts.** A
task reclaimed at `status='implementing'` with a saved `proposer_session_id`
skips planning and resumes implementation directly (`resume: proposerSessionId`,
now durable per #1). Every other reclaim (`claimed`, `planning`) restarts
planning from scratch — cheap and human-supervised, unlike implementation, which
is where the real incident died. Full bidirectional resume of an interrupted
planning debate is out of scope.

**5. `createWorktree` is idempotent, gated by an explicit `resume` flag.** Only
`true` when genuinely resuming (per #4). It reuses an existing worktree as-is
only if a valid one already sits on the right branch; otherwise it wipes and
recreates, so a dead attempt's partial edits never leak into a fresh attempt. It
always runs `git worktree prune` immediately before `git worktree add` — this
ordering matters: pruning *before* clearing the target directory left a stale
"missing but registered" worktree entry that broke the very next `add` (caught by
`worker/src/git.test.ts`, a real bug in the first implementation of this design).

**6. A 0-turn/$0 implementation result is classified transient**, matching the
actual incident shape, and thrown as a distinct `TransientError` rather than a
plain `Error`. The worker requeues it (`status='pending'`, worktree kept, the
same worker's next poll picks it back up) instead of failing outright, up to
`MAX_TASK_RETRIES` (default 2) attempts total — after which it fails terminally
with a clear "exceeded retry cap" message, checked immediately after claim so an
exhausted task does no wasted SDK work.

**7. A `lease_id`, re-minted on every claim/reclaim, guards the two genuinely
irreversible actions** (`pushAndOpenPr`, and `removeWorktree` on any terminal
exit) via a `stillHoldsLease` check immediately beforehand. This is not a full
distributed lock — it's a targeted check against one specific, rare failure
mode: a network partition (not a plain crash) where this worker's heartbeat went
stale, another claimant reclaimed the row, and this worker's connectivity then
recovers mid-execution. Under this repo's actual topology (one persistent pod
per repo, `Recreate` deploy strategy per `docs/adr/0003` — a plain crash/OOM
kills the whole process tree before Kubernetes restarts the container, so there
is no window for two live processes on the same task in the common case), full
mutual exclusion on every write was considered and rejected as disproportionate
to this residual risk.

## Consequences

- Closes the exact incident: a transient 0-turn/$0 result now retries instead
  of permanently failing, and a genuine pod crash mid-implementation now resumes
  the same Claude session instead of losing all context.
- `docs/adr/0003`'s "no automatic requeue... not yet automated" gap is closed.
- Session state vs. on-disk worktree state can still theoretically diverge if a
  crash lands exactly between a tool-call file write and server-side turn
  persistence — not fixable from this codebase (opaque to the Agent SDK).
  Accepted as a residual risk: nothing pushes until the final PR step, so the
  worst case is a conflicting edit a human catches before merge, not silent
  corruption of the shared branch.
- No graceful mid-task pause on SIGTERM was built — a killed process is treated
  identically to a crash and recovered via heartbeat-staleness reclaim, reusing
  one path instead of a second, complex, drain-aware one.
- `MAX_TASK_RETRIES` (env, default 2) and the 10-minute heartbeat-staleness
  window are both worker-process constants, not per-task configurable.
- `CLAUDE_CONFIG_DIR`'s exact behavior comes from reading the installed SDK's
  bundled source directly, not published documentation — worth re-verifying
  against current Anthropic docs if the SDK version changes materially.
