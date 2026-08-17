# ADR-0027: dashboard permission-mode selector (including `bypassPermissions`) and slash-command palette

**Status:** Accepted; the `allowDangerouslySkipPermissions` section superseded by
[ADR-0052](0052-auto-mode-and-the-bypass-launch-profile.md), which also adds
`auto` to the selector and makes `bypassPermissions` a launch profile rather
than a live switch. The selector and palette themselves stand.
**Date:** 2026-08-06

## Context

The web dashboard let Mohammad interact with a running task, but not on
equal footing with the interactive Claude Code CLI in three respects:

1. **Slash commands.** The CLI intercepts `/`-prefixed input locally and
   runs a matching skill/command. Tracing the bundled Agent SDK
   (`worker/node_modules/@anthropic-ai/claude-agent-sdk/cli.js`) confirmed
   this interception happens for *any* streamed-input text, not just
   interactive-REPL input — meaning the dashboard's existing free-text
   `Discuss` RPC already delivered working slash-command execution before
   this change. The actual gap was discoverability: nothing surfaced which
   commands exist.
2. **Permission mode.** The dashboard's `Approve` button was a single fixed
   `plan → default` flip (`core/internal/dashboard/server.go`'s `Approve`
   handler, unchanged by this ADR). The SDK supports a richer set of modes
   (`acceptEdits`, `dontAsk`, `bypassPermissions`, `delegate`), none
   reachable from the dashboard.
3. **A related trust issue**, not architectural but worth recording here
   since the fix landed in the same change: the model can narrate a fake
   approval ("good, user approved") in its own assistant text without any
   real approval happening. This is not a security bypass — the write/edit
   gate (`worker/src/planning.ts`'s `canUseTool`, ADR-0021) is code-enforced
   and untouched by what the model says — but the transcript UI didn't
   visually distinguish a real backend-verified event from the model's own
   prose.

Mohammad decided to close all three gaps in one pass, explicitly choosing
to expose `bypassPermissions` from the dashboard despite it deliberately
weakening ADR-0021's two-layer write/edit gate for whichever task opts in.

## Decision

### Additive `SetPermissionMode`, `Approve` untouched

Added `SetPermissionMode(task_id, mode)` as a new `DashboardService` RPC and
`TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE` as a new transcript entry type,
rather than extending `Approve` with a mode field. `Approve` stays exactly
as it was — a fixed `plan → default` flip matching Discord's binary
`/approve` (`core/internal/discord/handlers.go`, unaffected since it writes
the transcript directly, never through `dashboard.Server`). `mode` is
validated server-side against an allowlist (`acceptEdits`, `dontAsk`,
`bypassPermissions`) before being written — the value ends up in a real
`q.setPermissionMode()` call in the worker, so an unvalidated string would
let a malformed request reach the SDK directly.

### `bypassPermissions`: exposed, gated by typed confirmation, verified safe by source trace

`bypassPermissions` disables `canUseTool` for the rest of the task's
session. Traced the exact bundled SDK logic before deciding how to wire
it up:

- `t$7` (the `set_permission_mode` control-request handler) sets
  `mode: "bypassPermissions"` unconditionally — it only refuses if a
  remote settings kill-switch is active, never checks
  `isBypassPermissionsModeAvailable`.
- `wD7` (the per-tool permission decision) allows every tool when
  `Z.toolPermissionContext.mode==="bypassPermissions"` as its own,
  unconditional first disjunct — a separate branch from
  `mode==="plan" && isBypassPermissionsModeAvailable` (the interactive
  CLI's own "plan mode" shortcut).

Consequence: `q.setPermissionMode("bypassPermissions")` alone is sufficient
to disable `canUseTool` for the rest of the session — `worker/src/
planning.ts` does not and must not pass `allowDangerouslySkipPermissions`
in the initial `query()` options. `isBypassPermissionsModeAvailable` is
computed once at `query()` construction as `(initialMode==="bypassPermissions"
|| allowDangerouslySkipPermissions) && !killSwitch` and never recomputed —
if that flag were set from the start, the `mode==="plan" &&
isBypassPermissionsModeAvailable` shortcut would silently allow Write/Edit
from the very first turn of *every* session, before any human ever picks
bypassPermissions, which would be a strictly worse hole than the feature
this ADR adds. Not setting the flag closes that shortcut entirely while
leaving the explicit, human-triggered `bypassPermissions` path fully
functional — confirmed by literal source read, not inference from the
SDK's own TSDoc (which describes the flag as "required" — it's required
only for the interactive CLI's own plan-mode-with-a-shortcut UX, not for a
programmatic `setPermissionMode` call from streaming-input mode).

The dashboard requires the human to type the literal word `bypass` into a
confirmation modal (`BypassConfirmModal.tsx`, modeled on the existing
`ErrorModal.tsx` dialog pattern) before the request is sent — a plain
button click can't fire it.

### Command palette stays name-only

`SDKSystemMessage.slash_commands` (the system/init message the worker
already relayed, extended to forward `slash_commands`/`skills` instead of
dropping them) is `string[]` — bare command names, no descriptions or
argument hints available at runtime. The dashboard's palette is
accordingly a lean autocomplete, not a rich command browser.

### Narration hardening

Added one sentence to `worker/src/planning.ts`'s `taskPrompt()` instructing
the model never to claim an approval/answer/mode-change happened unless a
real tool result confirms it. On the dashboard side, `APPROVE`/`ABORT`/
`PERMISSION_MODE` transcript entries now render as compact system-log
lines (badge + muted text, matching the existing `SYSTEM`/`RESULT`
treatment) instead of falling through to the generic markdown prose bubble
that also renders the model's own narration — so a real backend-verified
event and the model's own claim about one are no longer visually
identical.

## Alternatives considered

- **Extending `Approve` with a `mode` field** instead of a new RPC —
  rejected: would have changed `Approve`'s wire contract for Discord's
  existing binary `/approve` path for no benefit, since Discord never
  needed anything beyond the fixed flip.
- **Passing `allowDangerouslySkipPermissions: true` in the initial `query()`
  options**, following the SDK's own TSDoc literally — rejected after the
  source trace above showed it isn't required for the explicit
  human-triggered path and would open a session-long hole via the
  `plan`-mode shortcut.
- **A rich command palette** (descriptions, argument hints) — rejected for
  now: the SDK's system/init message doesn't carry that data at runtime, so
  building it would mean either a second data source or guessing; revisit
  if the SDK ever exposes `SlashCommand`-shaped data on `SDKSystemMessage`
  itself.

## Consequences

- Four independent copies of the transcript type string↔enum map
  (`core/internal/dashboard/server.go`, `core/internal/coreserver/
  server.go`, `sidecar/internal/localapi/server.go`, plus the older MCP-era
  duplication already flagged before this change) all needed the new
  `permission_mode` case added by hand. Pre-existing duplication, not
  introduced by this change — still worth collapsing into one shared
  mapping at some point, but out of scope here.
- A live smoke test against a real Claude session (not just the mocked
  `planning.test.ts` suite) confirming `bypassPermissions` actually
  disables `canUseTool` end-to-end is recommended before this reaches
  production traffic, even though the source trace above is conclusive
  about the SDK's own logic — no live API spike was run as part of this
  change (no credentials available in the implementing session), so this
  is a call-out, not a hard gate that blocked the change.
- `plan`/`default` remain reachable only via the existing binary `Approve`
  — the new selector deliberately does not expose them, since Approve
  already covers that transition and duplicating it would just be two
  paths to the same state.
