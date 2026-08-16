# ADR-0052 — `auto` is the working mode; `bypassPermissions` is a launch profile

- **Status:** Accepted
- **Date:** 2026-08-16
- **Supersedes:** [ADR-0027](0027-arbitrary-permission-mode-and-command-palette.md)'s
  "must not pass `allowDangerouslySkipPermissions`" section, and the
  *placement* half of [ADR-0049](0049-project-setting-source.md)'s
  `permissions.ask` counterweight (the rules survive; the file they live in
  changes)

## Context

`00fefce` bumped `@anthropic-ai/claude-agent-sdk` 0.1.77 → 0.3.233 to survive
background tasks. It also replaced the CLI's permission evaluator, and three
things broke or changed underneath the fleet. All of the following was read out
of the shipped binary
(`node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude`), not inferred
from the SDK's TSDoc, which is wrong about at least one of them.

### The evaluator's new order

```
deny rules
ask rules                       ← q3n(ctx, tool, input, "ask")
tool.checkPermissions()         ← deny short-circuits here
content-level ask rules
tool.requiresUserInteraction()  ← ask
mode short-circuit              ← if (bypassPermissions || plan && available) allow
allow rules                     ← allowedTools
```

In 0.1.77 the mode was the unconditional *first* disjunct (ADR-0027's `wD7`
read). Everything below follows from it having moved down.

### 1. A live switch into `bypassPermissions` is refused

```js
if(e==="bypassPermissions"){
  if(cF()) return {ok:!1,error:"Cannot set permission mode to bypassPermissions because it is disabled by settings or configuration"};
  if(!t.isBypassPermissionsModeAvailable) return {ok:!1,
    error:"Cannot set permission mode to bypassPermissions because the session was not launched with --dangerously-skip-permissions"}}
```

`isBypassPermissionsModeAvailable` is computed **once, at launch**:
`S=(permissionMode==="bypassPermissions"||allowDangerouslySkipPermissions)&&!disabledByPolicy`.
The `PermissionUpdate {type:"setMode"}` route is gated identically ("Ignoring
permission update: setMode 'bypassPermissions' rejected"), so there is no
second door.

`worker/src/session.ts` swallowed the rejection into a `log("warn")` and then
ran `resolveAllPendingAllow()` regardless — the parked tool call was allowed,
`sessions.permission_mode` was already written, the dashboard badge flipped,
and the next tool call prompted again. A refused switch was indistinguishable
from a working one.

### 2. `permissions.ask` outranks every mode, including bypass

ADR-0049's 8 entries (`git push`, `gh`, `rm`, `sudo`, `kubectl`, `curl`,
`wget`, `env`) now match before the mode's own allow. A correctly-launched
bypass session could not `gh pr create` — the fleet's only deliverable.

### 3. `ExitPlanMode` and the native `AskUserQuestion` ask in every mode

Both declare `requiresUserInteraction()`, which sits above the mode
short-circuit **and** above allow rules — so `allowedTools` cannot silence them
either. No permission mode will ever stop the plan card from appearing.

### And the mode that was added: `auto`

A model classifier answers the ordinary prompts. It is **not** launch-gated;
`setPermissionMode("auto")` works mid-session. Its own gates are a remote
config circuit breaker and a model allow-list excluding `claude-3-*`,
opus-4-0/4-1/4-5, sonnet-4-0/4-5, haiku-4-5 — the fleet runs `claude-opus-4-8`.

It deliberately falls back to a human ask (telemetry:
`tengu_auto_mode_fallback_to_ask`) for ask-rule matches, `requiresUserInteraction`
tools, the plan-mode floor and an org ceiling. `auto` removes the ordinary
prompts, not the outward-facing ones.

## Decision

### `auto` is what plan approval switches to

Approving a plan is a **mode transition plus an answer**, mirroring the CLI's
own ExitPlanMode menu — "Yes, and use auto mode" / "Yes, manually approve
edits" / "No, keep planning". The dashboard's `PlanCard` and `DecisionInline`
offer exactly that, auto behind a `ConfirmModal`. `auto` also joins the
`ActionsMenu` picker, so a session can enter and leave it without a plan.

Order is load-bearing and lives in one place (`dashboard/src/approvePlan.ts`):
`SetPermissionMode` **then** `RespondToPermission`. The agent's next turn
starts the moment its `canUseTool` promise resolves, so a mode set afterwards
lands too late — that turn would still run in `plan`, where every write is
refused.

`core`'s `validPermissionModes` gains `"auto"`. That allowlist is the only
thing between the dashboard and a real `q.setPermissionMode()` call, so it is
the enabling change; a missing entry surfaces as `InvalidArgument` on a button.

### Crossing the bypass boundary is a relaunch, in both directions

The worker ends the pod — deny pending, say so in the transcript,
`interruptSafely()`, `abort()` — and the next warm boots into the mode `core`
already persisted. Same shape as `/clear`, minus `saveSessionId("")`: the
conversation must survive.

Both directions, because two different launch-time facts hang off that
boundary: *into* bypass the SDK refuses the switch, and *out of* bypass the ask
rules below are already absent, so a session that merely left bypass would keep
total authority with nothing on screen saying so. It also closes the residual
ADR-0027 named: bypass → `plan` without a relaunch would hit the
`plan && isBypassPermissionsModeAvailable` auto-allow.

Every other switch — `auto` included — stays a live control request. But a
rejection is now reported to the human instead of logged, and
`resolveAllPendingAllow()` runs only on success.

### `allowDangerouslySkipPermissions` is still not passed

ADR-0027's objection survives the version bump, re-verified rather than
assumed: with the flag set, `p = mode==="plan" && isBypassPermissionsModeAvailable`
is true from turn one, and plan mode's own block on writes is
`{behavior:"ask", "Cannot write to X while in plan mode."}` from
`checkPermissions` — an ask the `if(p) return allow` swallows. Passing it would
auto-allow Write/Edit/Bash in every planning session, without a human ever
choosing bypass. Strictly worse than the bug it would fix.

### The ask list moves into the worker

`FLEET_ASK_RULES` in `worker/src/session.ts`, passed via `query()`'s `settings`
option (`--settings`, scope `flagSettings`, inline JSON) — **omitted** when the
launch mode is `bypassPermissions`.

This works because the rule-source list is
`allowedSettingSources ∪ {flagSettings, policySettings}`: the rules apply
despite `settingSources: ["user","project"]`, and merge with the settings files
rather than replacing them. ADR-0049's counterweight is preserved exactly for
every other session — a flag-scope `ask` still outranks a target repo's
project-scope `allow`.

## Consequences

- Turning bypass on or off costs a re-warm. The dashboard says so; the
  conversation survives it. `auto` is the mode for "stop asking me about
  everything", and it needs no restart.
- A bypass session no longer prompts for `git push`/`gh`. That is the point,
  and it is now the *only* mode where those are unprompted.
- The fleet's authority ceiling is still one list, but it is code
  (`worker/src/session.ts`) rather than `fleet-shared/settings.json`. A guard
  test asserts the file has no `permissions.ask` key, because re-adding it
  there would silently re-break bypass and nothing else would notice.
- `ExitPlanMode` still costs a click in every mode. Unfixable from here — the
  fix was to make that click worth something, which is the auto transition.
- A session launched in bypass and then switched to `plan` **would** hit the
  plan auto-allow; the boundary rule is what prevents it, so it must stay
  bidirectional.

## Verification

- `worker`: a rejected switch must not resolve pending permissions and must
  post a system message; crossing the bypass boundary must not call
  `setPermissionMode`, must keep the saved session id, and must tell the human
  to re-warm; `settings.permissions.ask` present on a `default` launch and
  absent on a `bypassPermissions` launch. Plus the `fleet-shared/settings.json`
  guard.
- `dashboard`: `approvePlan` sets the mode before answering, and does not touch
  the mode for a plain approve.
- `core`: `validPermissionModes` covers every mode the dashboard can send.
- **Not verifiable in a worker pod, and deliberately not claimed here:** the
  live SDK behaviour. This pod has no `CLAUDE_CODE_OAUTH_TOKEN` (checked), so
  the four things only a real session shows — bypass refused from a `default`
  launch, bypass honoured at launch, `setPermissionMode("auto")` accepted on
  this model, and `gh` prompting with the injected rules but not in bypass —
  need `/kind-local` or a live session before this is trusted in production.
  Everything above is a source read of the shipped binary, which is how the
  bug was found but is not the same as watching it work.
