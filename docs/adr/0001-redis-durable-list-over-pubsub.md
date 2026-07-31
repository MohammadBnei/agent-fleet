# ADR-0001: Durable Redis list over pub/sub for the planning transcript

**Status:** Accepted
**Date:** 2026-07-30 (implementation; the choice was flagged as open in the original `mvp-spec.md` draft, which had defaulted to pub/sub)

## Context

The proposer, critic, and Mohammad's Discord replies all need to read and
write one shared planning transcript per task, coordinated through
`mcp-redis`. Redis pub/sub was the original default in `mvp-spec.md` v1 —
it was already the obvious "coordination bus" choice and Redis was already
running (Pigsty). But pub/sub has no delivery guarantee to a subscriber
that isn't actively listening at publish time: a message published while
the proposer session is mid-turn (not currently blocked on a subscribe
call) is simply lost.

That failure mode is unacceptable for a plan-mode approval gate — if
Mohammad's `/approve` or `/stop` can silently vanish because a session
happened to be busy, the gate isn't a real gate.

## Decision

Use a durable Redis **list** per task (`agentfleet:planning:<taskId>`),
written with `RPUSH` and read by polling `LRANGE` from a tracked cursor
index (`mcp-redis/src/index.ts`'s `wait_for_messages`, polled every 500ms;
`worker/src/planning.ts`'s `watchBatch`/`waitForCheckpointReply`, polled
every 1000ms). Every reader tracks its own `sinceIndex`/cursor, so a
message written while nobody is polling is simply picked up on the next
poll — nothing is dropped.

## Consequences

- No new Redis deployment — reuses the instance already running in
  `pigsty/`.
- Slight latency (up to one poll interval) versus true push delivery —
  acceptable; this is a human-paced approval conversation, not a
  low-latency data path.
- The list grows unboundedly per task; no TTL/trim is implemented yet.
  Acceptable at current task volume; revisit if the `tasks` table and its
  transcripts start accumulating enough to matter.
- Any future coordination need in this fleet defaults to this same
  RPUSH+poll pattern, not pub/sub, unless a new ADR justifies otherwise.
