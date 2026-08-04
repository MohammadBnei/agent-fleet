# ADR-0011: Critic session is opt-out (human-only), plus proposer→critic context handoff

**Status:** Superseded by [0017](0017-single-session-planning-pipeline.md)
**Date:** 2026-07-31

## Context

Two related inefficiencies surfaced once real tasks ran through planning:

1. Both proposer and critic sessions independently read the repo cold
   (`worker/src/planning.ts` prompts told each to "read the actual
   repository") — duplicate turns/cost re-deriving the same context every
   round.
2. The critic always runs, every round, regardless of task size. A
   one-line fix pays for a full independent repo crawl + critique just
   like a large feature does.

The Agent SDK's `agents` option (`runtimeTypes.d.ts`) lets a session define
subagents invoked via its own Task tool — this was considered as a way to
make the critic "a tool call" the proposer invokes only when it judges the
task warrants one. Rejected: a Task-tool subagent still starts with fresh
context (only what's in the calling prompt carries over, so it doesn't
solve #1 either), and letting the proposer decide *whether* to consult the
critic reintroduces the self-assessment bias the critic exists to catch —
in tension with ADR-0005's principle that safety/quality gates resolve on
an explicit human signal, never inferred agent judgment.

## Decision

- **Context handoff, no architecture change:** the proposer's prompt now
  asks it to cite the files/paths it read in its plan message. The
  critic's prompt now asks it to start from the proposer's cited findings
  (already in the shared transcript by the time it runs) and only
  Read/Glob/Grep further to verify a specific claim or cover a gap —
  instead of re-reading the repo cold. Still two independent
  `query()` sessions per ADR-0002; only the prompts changed.
- **Critic is opt-out, not conditional-by-agent-judgment:** `tasks` gained
  a `skip_critique BOOLEAN NOT NULL DEFAULT false` column
  (`db/schema.sql`), set via a `skip_critique` boolean option on the
  `/task` slash command (default false — critique runs). `runPlanningPhase`
  reads `task.skip_critique` and, when true, never spawns the critic
  `query()` loop for that task; round-cap math in `watchBatch` falls back
  to counting proposer turns alone (`criticEnabled` param). The decision
  is made once, by Mohammad, at task-creation time — never inferred by the
  proposer mid-run.

## Consequences

- Small/trivial tasks can skip the critic entirely at zero code-review
  cost, but only via an explicit human choice at `/task` time.
- Critic quality should improve on cost, not degrade: it still reads
  independently for verification, but doesn't have to rebuild the whole
  repo picture the proposer already built.
- If a future need arises to let the proposer *itself* decide when a
  critic is warranted, that's a different decision (agent-inferred gate)
  and needs its own ADR — this one deliberately keeps that call human-only.
