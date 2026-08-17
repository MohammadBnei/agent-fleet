# ADR-0053 — The gate is `canUseTool`, not a list the SDK re-interprets

- **Status:** Accepted
- **Date:** 2026-08-17
- **Supersedes:** [ADR-0052](0052-auto-mode-and-the-bypass-launch-profile.md)'s
  `bypassPermissions`-as-launch-profile half (the mode is deleted; `auto`,
  the ask list's placement, and the plan-approval ordering all stand)

## Context

Two things were reported from live use, one day after ADR-0052 shipped:

1. **`AskUserQuestion` and `send_message` prompted for permission.** The agent
   had to be granted permission before it could ask for permission — a
   permission card in front of the question form, on the one tool whose entire
   job is reaching a human.
2. **`auto` still asked.** ADR-0052 introduced it as "the mode for stop asking
   me about everything", and it did not deliver that.

ADR-0052's own Verification section said this would happen: *"Not verifiable in
a worker pod, and deliberately not claimed here: the live SDK behaviour."* It
shipped on a source read. This ADR is the follow-up that read further.

### Why the MCP allowlist never held

`worker/src/session.ts` allows the fleet's own tools with one entry,
`mcp__agent-fleet-sidecar__*`, in `allowedTools`. `ff10b49` (ADR-0048) deleted
the nine explicit per-tool entries beside it after checking the wildcard against
SDK **0.1.77**, where an allow rule was the second thing the evaluator
considered. `00fefce` then bumped to **0.3.233**, and allow rules moved to the
very bottom. The wildcard still matches — that was checked, in the shipped
`claude-agent-sdk-darwin-arm64/claude`, and is not the bug. Two gates above it
are:

```js
// non-read-only MCP tool + plan mode, before the allow-rule lookup
if (mcpInfo && !isReadOnly(input) && behavior === "passthrough" && mode === "plan" && !isSafeTool(name))
  l = { behavior: "ask", message: `Cannot call ${name} while in plan mode.` }
…
if (mcpInfo?.effectiveMaxPermission === "ask") return { behavior: "ask", … }   // org ceiling
```

`isReadOnly` for an MCP tool is its `readOnlyHint` annotation, which `mcp-go`
does not emit — so *every* sidecar tool is non-read-only, and *every* planning
session prompts for all of them. `effectiveMaxPermission` is an
organization-level cap, read off the server config, also above the allow rules.
Neither is reachable from `allowedTools`, from `permissionMode`, or from
anything else the fleet passes.

### Why `auto` still asked

By design, in the SDK: the classifier deliberately falls back to a human ask on
an ask-rule match (`tengu_auto_mode_fallback_to_ask`, reason `ask_rule`).
`FLEET_ASK_RULES` is eight entries wide — `git push`, `gh`, `rm`, `sudo`,
`kubectl`, `curl`, `wget`, `env` — and covers most of what a session does that
is worth doing.

### The shape both bugs share

Each is a decision the fleet had already made, expressed as **configuration an
evaluator we do not own re-interprets**. ADR-0052 was itself the second
consecutive ADR spent on this: three of its four findings were "the SDK moved
this line, so our policy changed underneath us."

## Decision

### The fleet's own tools are allowed in `canUseTool`, not by a rule

`FLEET_OWN_TOOL` — `mcp__agent-fleet-sidecar__*` and `mcp__playwright__*` —
returns `{behavior: "allow"}` from the top of `canUseTool`, before the rtk
rewrite and before any transcript entry is written. `allowedTools` keeps its
entries; that is still the fast path when the SDK agrees. This is the backstop,
and it is deliberately a *different mechanism* rather than a longer list.

### `auto` means auto: only `rm` and `sudo` still ask

In `auto`, `canUseTool` allows everything except a `Bash` whose command matches
`/(^|[\s;&|(])(sudo|rm)\s/`. Anything the SDK's own classifier allows never
reaches the callback at all; anything that does — an ask-rule match, a
classifier fallback, a passthrough — gets a deterministic fleet answer. Whether
the classifier runs is therefore no longer load-bearing, which is the part
ADR-0052 could not verify.

The match is on the command text, not a bash parse. `sudo` behind an `eval` or
a variable slips through, and that is accepted: this is a convenience gate on a
mode a human explicitly chose, not a sandbox. Marked `ponytail:` in the source
with tree-sitter-bash as the upgrade path.

### `FLEET_ASK_RULES` becomes unconditional

The one thing that ever varied it was the bypass carve-out. The rules keep
doing ADR-0049's job — a flag-scope `ask` outranking a target repo's own
project-scope `allow` — and *what an ask means* is now decided one layer down,
per mode, in code.

### `bypassPermissions` is deleted

With `auto` allowing everything but `rm`/`sudo`, bypass bought one thing: also
skipping those two. For that it charged a launch profile — the SDK fixes bypass
availability at startup, so ADR-0052 had to end the pod and re-warm on **both**
boundary crossings, and vary the ask list per pod. Gone:

- the mode from `core`'s `validPermissionModes` (asserted absent, not just
  removed — the map is what a hand-made request hits);
- the relaunch branch in the worker's `permission_mode` handler, so every
  switch is a live control request again;
- `BypassConfirmModal`, subsumed by the generic `ConfirmModal` it was the
  template for. `auto` inherits the confirm, since `auto` is now the mode that
  grants authority — one `AUTO_MODE_WARNING` string, because two surfaces show
  it and a stale copy is a human agreeing to the wrong thing.
- `db/migrations/000004` rewrites any stored `bypassPermissions` to `auto`. Not
  cosmetic: the column is what the *next* warm launches the pod in, so a
  rejected value at the API boundary does not cover a row already holding one.

## Consequences

- The fleet no longer asks permission to ask permission, in any mode.
- `auto` is a real grant of authority and is confirmed like one. The old typed
  `bypass` word is gone; a plain confirm gates it on both surfaces.
- `plan` mode no longer prompts for `journal_write`, `set_session_meta`,
  `list_sessions` and the rest — which it did for every planning session the
  fleet has ever run under 0.3.233.
- `ExitPlanMode` still costs a click in every mode. Unchanged and still
  unfixable from here (`requiresUserInteraction` sits above everything the
  fleet can set); the auto transition is what makes that click worth something.
- Two SDK behaviours are no longer load-bearing: whether the MCP wildcard
  matches, and whether the auto classifier runs. Both were, and both were
  wrong.

## Verification

- `worker`: a fleet MCP tool resolves `allow` with **no `permission_request`
  pushed** (the transcript entry is what a human sees, so that is the
  assertion); `auto` allows `gh pr create` and `Write` silently, still parks
  `rm -rf build` *and* `make build && sudo install`; switching to `auto`
  mid-session is a live `setPermissionMode` with no re-warm message, and the
  gate reads the new mode on the next call.
- `core`: `bypassPermissions` is absent from `validPermissionModes`.
- `dashboard`: `bun run build`, not `tsc --noEmit` — deleting a component is
  exactly the change `noUnusedLocals` catches and the weaker check does not.
- **Not claimed here, for the same reason ADR-0052 could not claim it:** the
  live SDK behaviour. What *is* claimed is that it no longer matters — every
  path this ADR changes is answered in `canUseTool`, in the fleet's own
  process. The remaining live check is a real session: `AskUserQuestion` with
  no card in front of it, then `auto` running `ls` silently and `sudo -n true`
  not.
