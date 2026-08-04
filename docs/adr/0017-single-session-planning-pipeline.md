# ADR-0017: Single-session planner replaces proposer/critic

**Status:** Accepted (Discord-as-interactive-user section superseded by [0018](0018-ask-user-question-via-dashboard.md) — everything else here still holds)
**Date:** 2026-08-04

## Context

Since `docs/adr/0002`, planning ran as two independent Agent SDK `query()`
sessions per task — a "proposer" and a "critic" — coordinating only through
the shared `planning_transcript`, with round-cap checkpoints
(`docs/adr/0008`) and a `/task`-time `skip_critique` opt-out
(`docs/adr/0011`). This bought real review coverage, but at real cost: two
independent turn/cost budgets for what is fundamentally one planning task,
a hand-rolled adversarial-debate prompt to maintain, and a human decision
(`skip_critique`) that had to be made blind, before any repo exploration
had happened.

Claude Code itself now ships general-purpose skills that do the same job
with less bespoke code: `doubt-driven-development` (a fresh-context
adversarial reviewer, spawned via the `Task` tool) and `architecture-
interview` (structured elicitation for non-trivial design decisions,
one question at a time). Reusing them means the fleet doesn't maintain its
own review logic — it inherits improvements to those skills for free.

## Decision

**One planner session, not two.** `worker/src/planning.ts`'s
`runPlanningPhase` runs a single `query()` session (`from: "planner"`)
instead of a proposer/critic pair. Its prompt describes a pipeline —
explore → review → interview (optional) → plan → doubt (optional) →
interview again (optional) → plan → wait for approval — and the planner
decides for itself, per task, whether the interview/doubt stages apply,
using each skill's own documented "when to use" criteria. There is no
`/task`-time override knob (`skip_critique` is dropped, not renamed):
Mohammad can still interject live in the Discord thread at any point
("skip the interview, this is trivial" / "run doubt on this") since the
interview steps already route through the same `send_message`/
`wait_for_messages` MCP tools as the approval gate — a live, reactive
override the old task-creation-time boolean never had.

**doubt-driven-development's `Task`-tool subagent replaces the critic
session structurally.** `Task` is added to the planning-phase
`allowedTools`. The subagent it spawns gets no write/edit access unless
explicitly granted — the prompt doesn't grant any — so ADR-0005's
"write/edit structurally absent from planning" property is unaffected.

**Revisiting ADR-0011's rejection of agent-inferred critique.** ADR-0011
explicitly rejected letting the proposer decide when to consult a critic,
reasoning that self-assessment bias undermines the reason a critic exists,
and that safety gates should resolve on explicit human signal
(ADR-0005) — not inferred agent judgment. This ADR delegates exactly that
judgment call to the planner. The distinction: ADR-0005's actual
safety-critical gate — write/edit only unlocks after Mohammad's explicit
`/approve` — is untouched here. What's being delegated is how much rigor
the plan-*drafting* process uses before Mohammad ever sees it, not whether
code gets written. And unlike the old blind `skip_critique` boolean (set
before any exploration, invisible once set), the planner's interview/doubt
reasoning is relayed live to Discord as it happens — Mohammad watches it
happen and can override it in real time, a stronger supervision property
than a pre-task guess ever had.

**Skill vendoring: a local plugin, not a marketplace install.**
`doubt-driven-development` and `architecture-interview` aren't published
as an installable marketplace plugin (unlike `ponytail`, ADR-0009) — they
exist as loose `~/.claude/skills/<name>/SKILL.md` files. `worker/skills/
agent-fleet-planning/` vendors verbatim copies as a local plugin
(`.claude-plugin/plugin.json` + `skills/*/SKILL.md`), loaded via
`plugins: [{ type: "local", path: ... }]` in the planning `query()` call —
not `settingSources: ['project']`, because the planning session's `cwd` is
the *target* repo's worktree, not this one; project-relative skill
discovery would look in the wrong place entirely. No sync mechanism exists
to track the upstream skills — a stale copy fails safe (worse guidance, not
broken behavior), the same tradeoff ADR-0009 already accepted for
`ponytail` in this same Dockerfile.

**Discord-as-interactive-user.** Both `architecture-interview` and
`doubt-driven-development`'s cross-model-escalation step assume a live,
responsive user reachable via `AskUserQuestion` — a tool that doesn't exist
in a headless `query()` session (no TTY, no `canUseTool` handler).
`architecture-interview`'s own "Loading Constraints" say not to invoke it
in non-interactive contexts at all. The fleet does have a live, responsive
human — Mohammad, via Discord — just reachable through a different
mechanism than the skill authors assumed. The planner's system prompt
explicitly reinterprets this: "the user" for interview purposes means
Mohammad via `send_message`/`wait_for_messages`, one question per
round-trip, and `AskUserQuestion` must never be attempted (it would be
silently denied, burning a turn for nothing — the same silent-permission-
denial trap `docs/adr/0008` was written after). Doubt-driven's
cross-model-escalation sub-step (shelling out to `gemini`/`codex`) is
explicitly treated as non-interactive here — skipped and announced, per
that skill's own self-degrade rule for CI/`/loop`/scheduled contexts; the
planner is never authorized to invoke an external CLI.

**Round-cap counting: `PLAN_READY:`-prefixed posts, not every message.** A
single planner session can emit many `send_message` posts before there's
anything to checkpoint on — each interview question is its own round-trip.
`watchBatch` counts messages prefixed `PLAN_READY:` (posted only when the
planner has a complete, reviewable plan) toward `MAX_PLANNING_ROUNDS`;
interview questions and doubt-cycle status still relay live to Discord for
visibility, they just don't advance the counter.

**Session-id persistence collapses to one column.** `docs/adr/0016`
introduced `tasks.proposer_session_id`/`critic_session_id` for crash
recovery. This ADR renames that to a single `planning_session_id` — it
does not revisit ADR-0016's recovery design (heartbeat reclaim, lease
guard, transient-error retry, `CLAUDE_CONFIG_DIR` on the RWX PVC), only
what it persists a pointer to.

## What's kept, unchanged

- ADR-0005's explicit human approval gate.
- ADR-0008's unbounded-by-default guardrail philosophy and the round-cap/
  checkpoint safety net.
- Same-session resume into implementation (no restart, no context loss).
- ADR-0016's crash recovery/heartbeat/lease/retry design in full.

## Consequences

- `docs/adr/0002` and `docs/adr/0011` are superseded — the proposer/critic
  split and the human-only `skip_critique` gate they describe no longer
  exist.
- Two things need live verification against a real target repo before this
  is considered fully proven, not just unit-tested (see `worker/src/
  planning.test.ts` for what's covered offline):
  1. Whether the `Task` tool is actually usable under `permissionMode:
     "plan"` — if silently denied the way write/edit intentionally are,
     doubt-driven's subagent spawn is dead on arrival with zero visible
     error. Check `session.result`'s logged `permission_denials` and the
     `system`/`init` message's `tools` array (now logged alongside
     `skills`/`plugins`, see `logSdkMessage`).
  2. ~~Whether the planner reliably follows the Discord-relay
     reinterpretation instead of attempting the nonexistent
     `AskUserQuestion` tool~~ — moot as of `docs/adr/0018`: `AskUserQuestion`
     is now a real tool, answered via the dashboard. The live-verification
     question is now the mirror image — whether the planner reliably *uses*
     it now that it's available — see that ADR's Consequences.
- Losing the ability to pre-empt a known-trivial task before planning
  starts (old `skip_critique=true` meant zero critic cost from turn one) —
  the new model only self-corrects reactively, after Mohammad sees the
  planner start down an overwrought path. Acceptable per the reasoning
  above; revisit if this proves annoying in practice.
- `tasks.skip_critique` and the `/task` Discord option are dropped
  outright, not deprecated in place.
