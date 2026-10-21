# ADR-0077 — Claude Code picks the default model; a repo session can pick model and effort

- **Status:** Accepted
- **Date:** 2026-10-21
- **Amends:** [ADR-0061](0061-sessions-launch-in-auto.md) §4 (the fleet no
  longer pins `claude-opus-5-5`), [ADR-0062](0062-defined-agents.md)
  (`model.default` is optional; §4's "`model.default` written explicitly"
  becomes "when set")

## Context

Every session ran a model the fleet chose. Core's fallback was
`claude-opus-5-5` (`defaultModel`, ADR-0061 §4), the worker had the same
literal as its own fallback, and `definitions/workspace-base.yaml` pinned
`model: { default: claude-opus-5, allow: [claude-opus-5, claude-sonnet-5] }`
for every definition. The owner wants the fleet to stop choosing: Claude Code
picks the default model, and an explicit pick is still validated.

`model.allow` on a definition was only ever checked against `model.default` at
load time. The worker never enforced `ModelAllow`, so the `allow` list bounded
nothing at run time.

The New Session dialog had no model or effort control, although
`CreateSessionRequest.model` existed and core validated it.

## Decision

1. **An empty model is passed as no model.** `fallbackModel()` returns
   `CLAUDE_MODEL`, the operator's override, and an empty value stores NULL.
   `defaultModel` is deleted. The worker spreads `model` into `query()` only
   when `session.model || CLAUDE_MODEL` is non-empty (`||`, so an empty string
   never reaches the SDK, ADR-0061 §4's point still holds), and the CLI decides
   otherwise. An explicit model on a repo session is still checked against
   `validModels`.
2. **A definition with no `model.default` stays NULL.** Core skips the
   fallback for a definition session, so its model comes from the definition
   alone. `model` is optional in a definition. When present, today's
   `default ∈ allow ⊆ ModelCeiling` check still applies.
   `workspace-base.yaml` drops its `model:` line, since keeping an `allow` the
   worker never enforced would be false comfort. A definition pod's worker still
   honours `CLAUDE_MODEL` if an operator sets it, the same override as for a
   repo pod.
3. **The row records what Claude actually picked.** `saveSessionId` reports the
   model from the SDK's `init` message, not the configured one. For a repo
   session the row then shows the CLI's choice. Core already ignores the report
   for a definition session (`SaveAgentSessionID`).
4. **The New Session dialog picks model and effort, for repo sessions only.**
   - `ListReposResponse.models` is `validModels` minus the dated legacy
     snapshots, so core's allowlist is the only list. The dialog's first option,
     "Claude's default", sends no model.
   - `CreateSessionRequest.effort` (field 9) is validated in core against the
     SDK's `EffortLevel` (`low|medium|high|xhigh|max`, the list definitions
     already use for `thinking.default`). Empty stores NULL. It is stored in
     `sessions.effort` (migration 000018) and carried to the worker on the
     `Session` proto, the same way as `permission_mode`.
   - The worker uses `session.effort || spec.thinkingDefault`. When it is set,
     it passes `effort` plus `thinking: { type: "adaptive" }`, as the definition
     path already did. When it is not, it passes neither.
   - A definition session takes neither from the caller: core refuses `effort`
     with a definition, as it already refuses `repo`, `model` and mode.
   - The session panel shows `effort` beside `model` and `mode` when one was
     picked.

## Consequences

- **A target repo's settings can choose the model.** With no explicit model, a
  repo's `.claude/settings.json` `model` (repo sessions load
  `settingSources: ["user", "project"]`) picks it, unchecked by `validModels`.
  That is Claude Code's own behaviour, and it can only pick models the
  credential allows.
- **A resume stays on the first pick.** After the first warm, a repo session's
  row holds the model the CLI picked, so a resume passes it explicitly.
- **Existing rows stay pinned.** Rows already stored as `claude-opus-5-5` keep
  it. Sessions are short-lived, so there is no data migration.
- **The default can differ by credential.** A subscription OAuth login (repo
  pods, `dev-notes`) and an API key (`opus-workspace`'s `ANTHROPIC_OPUS`) may
  default to different models. Check the `init` model in one pod of each after
  deploy.
