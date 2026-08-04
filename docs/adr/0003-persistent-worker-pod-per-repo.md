# ADR-0003: Persistent worker pod per target repo, not one Job per task

**Status:** Accepted
**Date:** 2026-07-30

## Context

`mvp-spec.md`'s original design (and the Success Criteria it defined) was
one Kubernetes **Job** per task: a trigger message spawns a fresh Job,
which runs to completion and exits, with worktree/PVC isolation coming
from the Job's own lifecycle. This gives clean per-task resource isolation
but means cold-starting a container (image pull, `git clone` of the full
target repo, MCP server boot) on every single task.

At implementation time this was revised: instead, one persistent,
always-on pod per target repo (`dream-analyst-worker`,
`vos-monolith-worker`) stays warm, holding one full clone of its repo, and
polls Postgres (`claimNextTask`, `SELECT ... FOR UPDATE SKIP LOCKED`) for
pending tasks. Isolation between concurrent/sequential tasks moves from
"a fresh pod per task" to "a fresh git worktree + branch per task"
(`worker/src/git.ts`'s `createWorktree`/`removeWorktree`), still against
that one already-cloned repo.

## Decision

One persistent worker pod per target repo. Git-worktree-per-task isolation
happens inside that pod, not at the pod-lifecycle level. `SKIP LOCKED`
guarantees two pods (or a restarted pod racing its predecessor) never
double-claim the same task row.

## Consequences

- No per-task pod cold-start cost — the repo clone and MCP server startup
  happen once per worker lifetime, not once per task.
- A worker pod crash mid-task leaves that task `claimed`/`planning` in
  Postgres with no automatic requeue — currently a manual fix (see
  `/fleet-debug`), not yet automated. **Closed by
  [0016](0016-task-crash-recovery-and-retry.md):** heartbeat-staleness reclaim
  now automates this.
- Adding a new target repo means adding a new worker deployment
  (`k8s/<repo>-worker.yaml`), not just a queue entry — see `/fleet-ops`.
- `mvp-spec.md`'s Success Criteria describing "exactly one Kubernetes Job
  being created" per trigger message is stale; no Job is created per task
  anymore. This is documented here specifically because it was a silent
  divergence from the written spec, caught only in a `README.md` aside —
  a lesson to keep `ARCHITECTURE.md` in sync with real deploys going
  forward, not just at design time.
