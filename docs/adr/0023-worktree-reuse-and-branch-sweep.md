# ADR-0023: Worktree/branch lifecycle redesigned around explicit signals only

**Status:** Accepted
**Date:** 2026-08-05

## Context

The provisioner owns the entire git lifecycle on the shared worktree PVC
(ADR-0019 point 2). Until now, that lifecycle used a task's `status` as a
proxy for "safe to delete" — and that proxy was wrong in two concrete,
data-losing ways, both caught by a reliability audit (`docs/
reliability-findings.md` finding #2), not by a live incident:

- `CreateWorktree` unconditionally `os.RemoveAll`'d the worktree path on
  every call, including a same-task-ID retry after a transient dispatch
  failure. Commits already made in that worktree survive (they live in
  the shared object database, and the branch-reuse logic below already
  relied on this correctly) — but any *uncommitted* work in progress at
  the moment of a crash-retry was destroyed for no reason, since the
  branch itself was already being correctly reused.
- `RemoveWorktree` unconditionally ran `git branch -D` on every terminal
  task status, including `failed`. A task reaching `failed` via a `git
  push` failure has commits that were **never pushed anywhere** — the
  branch ref inside the shared worktree PVC's object database is the
  *only* reference to them. Deleting it on exactly the status a push
  failure produces permanently destroyed the work the whole task existed
  to produce.

Both bugs share one root cause: using a task's terminal status, a signal
about the *task*, to make an irreversible decision about *git state* that
the status was never actually correlated with.

## Decision

1. **`CreateWorktree` reuses an existing worktree as-is — zero git
   commands, zero validity check.** If the path already exists, return it
   immediately. Stale `.git/index.lock` files, half-written work from a
   prior attempt — that's Claude Code's own problem to notice (it has
   Bash access inside its own pod, and can run `git status`/`git log`
   itself), not the provisioner's job to pre-empt by wiping first and
   asking questions never.

2. **Teardown (`grpcserver.tearDownWorker`) stops touching git state
   entirely, for every terminal status including `done`.** It deletes the
   worker's `Job` (ADR-0022) and reports the termination event; it no
   longer calls `RemoveWorktree` at all. Worktree/branch cleanup moves to
   two independent, explicit-signal-only paths that replace the
   status-triggered one:

   - **Automated: a periodic sweep** (new `provisioner/internal/sweep`
     package). Per repo: `git fetch --prune origin` first — deliberately
     **outside** the per-repo mutex, since it's network I/O and shouldn't
     stall a live `CreateWorktree`/`EnsureRepoCloned` call on the same
     repo — then `git for-each-ref --format='%(refname:short)
     %(upstream:track)' refs/heads/agent/`, looking for branches whose
     upstream tracking state is `[gone]` — i.e. their remote branch was
     deleted, meaning the PR was actually merged or closed. For each: the
     mutex is taken, then `git worktree remove` runs **before** `git
     branch -D` — order matters, git refuses `-D` on a branch that's
     still checked out in a worktree, so the wrong order would silently
     delete nothing.
   - **Manual: a dashboard "Worktrees" view.** New `ListWorktrees`/
     `DeleteWorktree` RPCs on `ProvisionerService`, proxied through a
     matching pair on `DashboardService`. `core` has no PVC access of its
     own (ADR-0020 point 1), so `ListWorktrees`' response is built by
     **left-joining** the provisioner's raw worktree list against
     `core`'s own `tasks` table — left, not inner, because a worktree
     whose task row no longer matches anything is exactly the orphaned
     case this view exists to surface; an inner join would hide it.
     `DeleteWorktree` takes an explicit `also_delete_branch` flag as a
     separate human choice, since a person clearing disk space might
     want the worktree gone without also destroying the branch.

## Alternatives considered

There was no serious alternative to the reuse-vs-wipe fix itself — the
old behavior was a bug with an obvious correct shape, not a real design
choice. The one real fork was **automated-sweep vs. manual-only cleanup**,
and both were kept rather than picking one: the sweep handles the common
case (a PR merged or closed) without a human having to notice, and the
dashboard view handles everything the sweep structurally can't reach (see
the accepted gap below), plus gives a human a way to intervene early if
the sweep interval feels too slow for a specific case.

## Consequences

- **Accepted gap, not fixed here**: a worktree/branch abandoned *before*
  its first push never gets an upstream tracking ref to compare against —
  `git worktree add -b <branch> <path> origin/<base>` tracks
  `origin/<base>` by git's own default, not a branch that doesn't exist
  yet on the remote. `[gone]` can therefore never fire for it, and it
  leaks forever, invisible to the sweep. The dashboard's manual delete is
  the primary cleanup path for this specific case; an optional, dumber
  mtime-based cron sweep (no `core`/Postgres lookup needed, keeping the
  provisioner's zero-DB-access property intact) is the fallback if manual
  cleanup proves insufficient in practice. Not solved by making the
  git-based sweep smarter — there's no git-visible signal to make it
  smarter *with*.
- `RemoveWorktree` (the underlying git.Manager method — unconditional
  `worktree remove` + `branch -D`) still exists and is still correct to
  call from the sweep (once `[gone]` is confirmed) and from the
  dashboard's explicit `DeleteWorktree` action; it's only `tearDownWorker`
  that stopped calling it.
- The provisioner gains a second background loop (`sweep`, alongside
  `reconcile`) — same per-repo-mutex-serialized git access `git.Manager`
  already provides, no new concurrency model.
- A worktree or branch is now never deleted as a side effect of anything
  else — only an explicit, confirmed signal (a real `[gone]` tracking
  state, or a human clicking delete) removes git state. This is now a
  named forbidden pattern (`docs/DECISIONS.md` §2) precisely because the
  old design violated it twice, silently, before anyone noticed.
