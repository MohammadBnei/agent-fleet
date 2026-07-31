# ADR-0008: Guardrail defaults are unbounded, capped only opt-in

**Status:** Accepted
**Date:** 2026-07-31

## Context

`worker/src/planning.ts`'s inline comments record the actual history: fixed
`maxTurns` defaults were tried at 15, then 40, then 100 turns for the
planning phase, and all three turned out too tight for genuine exploration
of an unfamiliar codebase. Confirmed live: the proposer once burned all 15
turns tracing a real Prisma panic across 4+ files and never even reached
its first `send_message` call — the debate never started, the round-cap
checkpoint fired on an empty transcript, and the failure was
indistinguishable from a hung session without checking turn count after
the fact.

A related but separate incident: `REDIS_MCP_TOOLS` (the `mcp-redis` tool
names) were initially allowlisted for the planning phase only. In a
headless `query()` with no `canUseTool` handler and no TTY, a tool missing
from `allowedTools` doesn't error loudly — its permission request is
silently denied. The critic burned all 15 turns (and $1.1+) on denied
`send_message`/`wait_for_messages` calls in under a minute, with zero
transcript entries on either side, before this was diagnosed.

## Decision

`MAX_TURNS_PLANNING`, `MAX_TURNS_IMPLEMENTATION`, and
`PLANNING_TIMEOUT_MS` are all **unbounded by default** — set only if a
specific run needs capping, via env var. `MAX_PLANNING_ROUNDS` stays
tightly capped by default (`1`) since that guardrail is cheap to lift (ask
Mohammad for another round) and expensive to leave uncapped (two agents
debating unsupervised). Every SDK message streams to logs/Discord live
(`logSdkMessage`) instead of only the final result, so a permission denial
or turn exhaustion is visible in real time, not reconstructed after the
fact from cost/turn-count.

## Consequences

- A genuinely stuck session (bad prompt, infinite tool-call loop) can now
  run far longer before self-terminating than under the old fixed caps —
  the round-cap checkpoint and the `/stop` kill switch are the actual
  safety nets, not `maxTurns`.
- Adding any new MCP tool must update `REDIS_MCP_TOOLS` (or whatever
  allowlist is relevant) for **every** phase that needs it, both planning
  and implementation — this exact gap is what caused the incident above.
  See `/fleet-feature`.
- Diagnosing a stuck task should always start with `knowledge_journal`'s
  `permissionDenials` count and `kubectl logs`, not an assumption that a
  long-running session is broken — see `/fleet-debug`.
