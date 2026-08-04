---
name: fleet-feature
description: Checklist for adding functionality to agent-fleet itself (bot/, worker/, or mcp-redis/) — new MCP tool, new slash command, new task status, new planning-phase behavior. Use when the user wants to extend the fleet's own code, not a target repo it manages.
user-invocable: true
allowed-tools:
  - Read
  - Edit
  - Write
  - Bash(bun run typecheck)
  - Bash(git diff *)
  - Bash(git status *)
---

# /fleet-feature — extend agent-fleet's own bot/worker/mcp-redis

This is for changing this repo's own source, not for a task a worker runs
against a target repo. See `docs/ARCHITECTURE.md` for the full flow and
`docs/adr/` for why things are shaped the way they are before changing them.

## Adding a new MCP tool

1. Define it in `mcp-redis/src/index.ts`'s `ListToolsRequestSchema` handler
   and implement it in `CallToolRequestSchema`.
2. **Allowlist it in `worker/src/planning.ts`'s `REDIS_MCP_TOOLS`** — a tool
   missing from `allowedTools` isn't just unavailable, its permission
   request is *silently denied* (no `canUseTool` handler, no TTY, headless
   `query()`). This exact gap burned a real session on denied calls with
   zero transcript output (`docs/adr/0008`).
3. Check whether the tool is needed in **both** planning and implementation
   phases — `REDIS_MCP_TOOLS` is shared across both `query()` calls in
   `runPlanningPhase`/`runImplementationPhase`, but if you ever split tool
   sets per phase, don't forget the phase where the resumed planner
   session still needs it.

## Adding a new Discord slash command

Add a `SlashCommandBuilder` to `bot/src/index.ts`'s `commands` array and a
handler branch in the `Events.InteractionCreate` listener. Commands are
registered guild-scoped (derived from `DISCORD_TRIGGER_CHANNEL_ID`'s guild)
on `ClientReady`, not globally — so a new command shows up on next bot
restart, not after up to an hour. If registration silently fails, the bot
likely wasn't invited with the `applications.commands` OAuth2 scope.

## Adding a new task status

Edit `db/schema.sql`'s `tasks_status_check` constraint using the existing
`DROP CONSTRAINT IF EXISTS` / `ADD CONSTRAINT` pattern — not an inline
column change. This keeps the migration idempotent and safe to re-run
against the already-live table (it's applied via a PreSync ArgoCD hook on
every sync, see `k8s/agent-fleet-bot.yaml`), not just on fresh creates.

## Adding a new planning-phase behavior (new checkpoint, new guardrail, etc.)

Read `worker/src/planning.ts` in full first — `runPlanningPhase`'s loop
structure (batch → `watchBatch` → checkpoint-or-return) and the
`WatchOutcome`/`SessionFlags` types are the load-bearing state machine.
`SessionFlags` is single-flag (`{ sessionEnded: boolean }`) as of
`docs/adr/0017` — planning is one `query()` session (`from: "planner"`),
not a proposer/critic pair. Prefer extending `WatchOutcome`'s variants over
adding parallel ad-hoc booleans. Any new guardrail should default to
**unbounded/opt-in**, not a fixed cap — `docs/adr/0008` documents why fixed
defaults (15, 40, 100 turns) were all tried and repeatedly proved too tight
for genuine exploration.

**Interview/doubt gating logic belongs in `plannerPrompt`, not
`watchBatch`.** Whether `architecture-interview`/`doubt-driven-development`
run is the planner's own judgment call, described in its prompt text —
`watchBatch` only sees `PLAN_READY:`-prefixed `send_message` posts (the
round-cap counting convention) and has no visibility into which pipeline
stages actually ran for a given round. Don't add a new boolean/env flag to
gate this — `docs/adr/0017` deliberately dropped the old `skip_critique`
`/task`-time knob in favor of the planner deciding per task, with Mohammad
able to interject live in the thread instead.

## Before committing

- `cd bot && bun run typecheck` / `cd worker && bun run typecheck` /
  `cd mcp-redis && bun run typecheck` — the only automated check that
  exists (no test framework; the golden path itself is the test, per
  `docs/ARCHITECTURE.md` §4).
- If the change touches `worker/Dockerfile` or `bot/Dockerfile`, remember
  both build from the repo root as context (`mcp-redis/` is copied as a
  sibling into `worker/`'s image) — `docker build -f worker/Dockerfile .`,
  not `docker build worker/`.
