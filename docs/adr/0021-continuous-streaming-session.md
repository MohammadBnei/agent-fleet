# ADR-0021: One continuous streaming-input session per task, live setPermissionMode() on approval

**Status:** Accepted
**Date:** 2026-08-04

## Context

ADR-0020's sidecar gave the worker's TS wrapper a local channel for its own
control-flow calls, described there as "the live poll for a human's
`/approve`/`/stop`... which decides whether to let the session continue or
`abortController.abort()` it." Pressed on whether that's actually the full
depth of human↔Claude Code interaction this fleet needs, the answer is no —
and the Agent SDK already has purpose-built support for the deeper version
that today's implementation (and ADR-0020 as first written) doesn't use.

Checked directly against the installed SDK's type declarations
(`worker/node_modules/@anthropic-ai/claude-agent-sdk/entrypoints/sdk/
runtimeTypes.d.ts`, `coreTypes.d.ts`), not assumed: `query()`'s `prompt`
accepts `string | AsyncIterable<SDKUserMessage>`. In the streaming-input
form, the returned `Query` exposes `interrupt()` ("interrupt the current
query execution... return control to the caller"), `streamInput(stream)`
("stream input messages to the query... used internally for multi-turn
conversations"), and `setPermissionMode()`/`setModel()`/
`setMaxThinkingTokens()` — all explicitly documented as available **only**
in streaming-input mode. `worker/src/planning.ts` uses the plain-string
form today, for both phases, which forfeits all of this: `/stop` is a
blunt `abortController.abort()`, "resuming" means tearing down one
`query()` call and starting a new one with `resume: planningSessionId`
(disk-based reload), and the agent only ever sees new human input when it
proactively calls `wait_for_messages` itself — a request-response tool
call, not a live conversation.

## Decision

1. **One continuous `query()` call, in streaming-input mode, spans an
   entire task's planning *and* implementation** — never torn down and
   restarted at the approval boundary. This is a materially more literal
   version of ADR-0017's own claim ("same planner session resumed into
   implementation — no restart, no context loss"), which today is achieved
   only via disk-based session resume across two separate process
   invocations.

2. **The sidecar (ADR-0020 point 5's third responsibility) delivers every
   new human message to the wrapper live**, and the wrapper feeds it into
   the running session via `streamInput()` — not a poll the wrapper
   initiates on its own schedule. This corrects ADR-0020's original
   description of that responsibility, written before this SDK capability
   was checked.

3. **`/stop` (and the word-matching `isAbort` fallback) maps to
   `query.interrupt()`**, replacing `abortController.abort()`. This is the
   SDK's own graceful stop primitive, not a process-level kill.

4. **`/approve` maps to a live `query.setPermissionMode()` call**, made by
   the wrapper the instant an explicit approval is confirmed (same
   `isApproval()` detection logic as today — ADR-0005's "never inferred
   from silence" rule is unchanged, only the mechanism that acts on it
   changes). No new process, no session teardown.

5. **The round-cap checkpoint (ADR-0008) becomes an in-session
   interrupt-and-wait, not a session teardown-and-restart.** The wrapper
   still counts `PLAN_READY:` posts; after `MAX_PLANNING_ROUNDS` without a
   verdict, it calls `query.interrupt()` (pausing, not ending, the same
   session), posts the checkpoint message, and resumes feeding input via
   `streamInput()` once a human reply arrives — continue, approve
   (`setPermissionMode()`), or stop (let the interrupt stand, tear down).

6. **`resume: sessionId` (disk-based) is not eliminated** — it remains
   exactly ADR-0016's crash-recovery path: a *new* pod, spawned by the
   provisioner after the original crashes (workers are single-shot per
   ADR-0019), resumes the original Claude Code session from disk. It's no
   longer used for the normal-path planning→implementation transition,
   only for recovering a session that's no longer running anywhere.

7. **`Write`/`Edit` stay OUT of `allowedTools` for the whole session,
   exactly as today** — confirmed empirically (see verification note below)
   that `canUseTool` is only ever consulted for tools *not* pre-authorized
   by `allowedTools`; a tool that's already on the list is auto-allowed and
   `canUseTool` never fires for it at all. This means the "declare the
   tools up front since there's no live `allowedTools` escalation" reasoning
   this point originally used was solving a problem that doesn't exist —
   `canUseTool` *is* the live escalation mechanism, and it works precisely
   because the tool is left undeclared. The wrapper registers one
   `canUseTool` (`Options.canUseTool`) for the session's whole lifetime
   that checks its own in-memory `approved` flag — denying `Write`/`Edit`
   attempts while `approved` is `false`, allowing them once flipped to
   `true` at the exact moment `setPermissionMode()` is called on confirmed
   human approval. No narrower defense-in-depth than today, no trade-off to
   accept: `permissionMode: 'plan'` still means the model essentially never
   even attempts a write during planning (confirmed: it resisted a
   deliberate prompt-injection-style override attempt, citing an injected
   system-reminder), and `canUseTool` is a second, SDK-enforced backstop if
   it ever did — confirmed as a real enforcement, not decorative, in an
   isolated test where the model attempted `Write`, `canUseTool` denied it,
   and the file was never created. ADR-0005's text needs a follow-up
   correction to name `canUseTool` as an additional real enforcement
   mechanism, alongside — not replacing — `allowedTools`.

   **Verified for real** (Phase 0 of the implementation plan, not just
   asserted from the type signature) via two standalone scripts run against
   a live session: (1) with `Write` *in* `allowedTools`, `canUseTool` was
   never invoked in either round — confirmed the bypass. (2) with `Write`
   *absent* from `allowedTools`: in `plan` mode the model wouldn't attempt
   it even under an adversarial prompt; in `default` mode with
   `approved=false` it did attempt it, `canUseTool` denied it, and the file
   was genuinely never created; after `setPermissionMode('default')` +
   flipping `approved=true`, the same session wrote the file successfully.
   `query.interrupt()` was also confirmed non-terminal — the same `Query`
   accepted further `streamInput()` afterward.

## Alternatives considered

- **Keep two separate `query()` calls, add streaming input only within
  each phase** (this session's own smaller-scope option, offered
  alongside this one). Rejected by explicit choice: the goal is "a real
  Claude Code" feel in the dashboard, which this smaller option doesn't
  fully deliver — the approval boundary would still be a hard process
  restart rather than a live mode switch.
- **Keep `abortController.abort()` for `/stop`.** Rejected: cruder than
  the SDK's own purpose-built `interrupt()`, which is documented to return
  control gracefully rather than tearing down the process.
- **Split `allowedTools` across a resume boundary** (today's model).
  Rejected as inapplicable once there's no session boundary left to split
  it at, and confirmed there's no live tool-list API to escalate mid-session
  instead.
- **Rely on `permissionMode: 'plan'` alone to gate `Write`/`Edit`** (this
  ADR's own first-drafted answer, flagged and questioned before being
  accepted). Superseded by adding `canUseTool` as a second layer — but not
  in the form first proposed.
- **Declare `Write`/`Edit` in `allowedTools` from session start, gated by
  `permissionMode` + `canUseTool`** (this ADR's own second-drafted answer,
  reasoning that there's no live way to escalate `allowedTools` so the tool
  must be pre-declared). **Empirically falsified in Phase 0's spike, not
  just reconsidered**: `canUseTool` was never invoked at all when `Write`
  was in `allowedTools` — the list bypasses the callback entirely, silently
  removing the layer this option was trying to add. Rejected once tested,
  in favor of point 7's actual verified design: leave `Write`/`Edit`
  undeclared (identical to today), let `canUseTool` be the live escalation
  path instead of `allowedTools` needing to change at all.

## Consequences

- `worker/src/planning.ts` needs a substantial rewrite — `runPlanningPhase`
  and `runImplementationPhase` collapse into one continuous-session driver;
  `watchBatch`/`waitForCheckpointReply`'s polling model is replaced by
  consuming the sidecar's live message feed.
- **ADR-0005 needs a small follow-up correction**: its enforcement
  mechanism is unchanged in substance (`Write`/`Edit` stay structurally
  absent from `allowedTools` during planning, exactly as today) but gains a
  second, independent layer worth naming — a `canUseTool` callback checking
  an explicit `approved` flag, confirmed by real testing to be a genuine
  SDK-enforced backstop, not decorative. The approval-gate *behavior*
  (never inferred from silence, explicit human act required) is unchanged.
- **ADR-0008's round-cap description needs a follow-up correction**: from
  "session ends, checkpoint posts, a new batch resumes it" to "session
  pauses via `interrupt()`, checkpoint posts, the same session resumes via
  `streamInput()`."
- **`query.interrupt()` non-terminality: confirmed, not just inferred.**
  Phase 0's spike reached a second round of `streamInput()` after calling
  `interrupt()` on the same `Query` object, with no restart — the SDK
  docstring's "return control to the caller" reading was correct.
- `resume: planningSessionId`'s role narrows to ADR-0016's crash-recovery
  path only — worth double-checking that path still works correctly now
  that a *resumed* session also needs to reconstruct streaming-input mode
  (not just re-attach to a plain-string session), since it was never
  exercised that way before this ADR.
