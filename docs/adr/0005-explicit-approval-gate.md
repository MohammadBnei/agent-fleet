# ADR-0005: Explicit human approval only gates write/edit unlock

**Status:** Accepted
**Date:** 2026-07-30

## Context

The core safety property of this fleet is that a worker never writes code
without Mohammad having actually looked at the plan. Several weaker
alternatives were implicitly available: unlock write/edit after N rounds
of debate with no objection, after a timeout with no explicit "stop", or
whenever the proposer and critic both stop raising new points.

## Decision

Write/edit tools are structurally absent from the planning-phase
`allowedTools` list (`worker/src/planning.ts` — `permissionMode: "plan"`,
tools limited to `Read`/`Glob`/`Grep`/`Bash`/`WebSearch`/`WebFetch` plus
the two `mcp-redis` tools). The implementation phase only ever starts after
`isApproval()` returns true for a message explicitly `from: "human"` — a
`/approve` slash command (`type: "approve"`, checked first, unambiguous)
or a word-matched plain-text reply ("approved"/"lgtm"/"ship it"/"go
ahead"). Round-cap and session-end both post a checkpoint and **block**
(`waitForCheckpointReply`) rather than auto-proceeding. `/stop` (or a
word-matched "stop"/"abort"/"cancel"/"kill") is checked before the approval
match and works at any point in either phase, not just at a checkpoint.

## Consequences

- Planning can run indefinitely without Mohammad's input beyond the round
  cap — by design (`PLANNING_TIMEOUT_MS` defaults to 0/unbounded), paced by
  him, not a clock.
- A worker can never "talk itself into" write access — there is no code
  path where write/edit tools become available without a message that
  satisfies `isApproval()`.
- Any new approval mechanism (e.g. a Discord reaction) must still resolve
  to an explicit `from: "human"` transcript entry with `isApproval()` true
  — never a heuristic on debate content or elapsed time.
