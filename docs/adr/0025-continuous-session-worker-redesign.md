# ADR-0025: Worker session is a plain Claude Code session — Discord/dashboard are transport only

**Status:** Accepted
**Date:** 2026-08-05

## Context

ADR-0021 already established one continuous streaming-input `query()`
session per task, with `interrupt()`/`setPermissionMode()`/`streamInput()`
as the live control surface. On top of that real capability, the fleet
had still built its own protocol layer: a fleet-orchestrated phase state
machine (`plannerPrompt`/`implementPrompt` as two distinct prompts,
`permissionMode` flipped by fleet logic), magic-string phase signals
(`PLAN_READY:`/`PR_READY:` prefixes the wrapper parsed out of free text),
a round-cap checkpoint whose entire premise was a phase boundary to pause
at, and free-text intent inference (`isApproval`/`isAbort` regex
word-matching) standing in for what Claude Code already does natively
with structured tool calls and permission prompts.

A reliability audit (`docs/reliability-findings.md` finding #0, with
findings #3/#4/#8 as sub-parts) found this wasn't just extra code — it
was actively wrong in ways a live incident would eventually have
surfaced: `isApproval`'s regex matched the substring "approve" inside "I
don't approve this, redo the auth flow," a genuine false-positive unlock
with no guard against re-firing mid-implementation; `PR_READY:` had no
error path on a mismatch, silently dumping the raw session transcript
into a PR body instead; and only the post-approval code path ever checked
an SDK result's `subtype`, so a non-success result during what the fleet
called "planning" went completely unhandled.

The guiding principle, stated plainly: Claude Code already handles
permission prompts, questions, and stop signals — headless included. The
fleet's job is to relay the real session through Discord and the
dashboard uniformly, not build a second protocol on top of what the SDK
already does. One open question blocked writing any of this: whether
holding a permission decision pending for the length of a real human
response time (minutes, not seconds) would hit any SDK or transport
timeout. A throwaway script, run against the live SDK, held a
`canUseTool` decision pending for 3 minutes — no timeout, no dropped
connection, the session continued normally once resolved. That result is
what let this redesign proceed as designed rather than needing a
keep-alive workaround.

## Decision

One PR, six points, all serving the same principle:

1. **`plannerPrompt`/`implementPrompt` merge into one `taskPrompt`.**
   There is no fleet-imposed phase inside a task's session anymore — the
   agent decides for itself, using its own judgment, how much
   explore/review/interview/plan/doubt process a given task actually
   needs, and when it's ready to implement. The only hard gate left is
   structural: `Write`/`Edit` are denied until approval, described in
   point 3.

2. **`PLAN_READY:`/`PR_READY:` parsing and the round-cap/checkpoint
   machinery (`awaitingCheckpointReply`, `MAX_PLANNING_ROUNDS`) are
   deleted outright, not hardened.** There's no longer a phase boundary
   for either to signal — `PLAN_READY:`'s only job was marking a
   checkpoint that no longer exists, and the round cap's only job was
   forcing a pause at a boundary that no longer exists. This finding is
   the direct resolution of the previously-open `docs/
   reliability-findings.md` findings #3 (`isApproval`/`isAbort`
   substring-matching, see point 3) and #4 (`PLAN_READY:`/`PR_READY:`
   magic strings) — both are superseded by deletion, not by a patch.

3. **`isApproval`/`isAbort`'s free-text regex fallback is deleted.**
   `/approve` and `/stop` remain, but only as already-structured signals
   (`type:"approve"`/`"abort"`) — free text is just conversation now,
   never auto-triggering anything. **The actual enforcement mechanism —
   `Write`/`Edit` structurally absent from `allowedTools`, `canUseTool`
   as the live escalation path checking an in-memory `approved` flag — is
   explicitly unchanged from ADR-0021.** Only the *trigger* that flips
   `approved` changed: a clean structured signal instead of a regex
   matched against arbitrary conversation text. ADR-0005's "never
   inferred from silence" rule is honored more strictly now, not less —
   a regex match against free text was itself a form of inference this
   redesign removes.

4. **AskUserQuestion gains question-seq correlation.** `planning_transcript`
   gains a nullable `reply_to_seq` column; `transcript.Store` gains a
   second method, `AppendReply(..., replyToSeq int64)`, alongside the
   existing `Append` (a second method, not a signature change, so the
   ~6 existing `Append` call sites that never need this didn't all need
   a meaningless zero value). `AskUserQuestion`'s own poll loop now
   matches an answer by `ReplyTo == its own question's seq`, not "any
   `type:"answer"` entry from a human" — ADR-0018 described the old
   single-pending-question assumption as sufficient; this redesign
   verified it as a real gap, not a hypothetical one, since nothing
   actually prevented an unrelated reply from satisfying a blocked call.
   The same correlation is what makes **Discord able to answer a
   question at all now** — a new `findPendingQuestionSeq` helper lets
   `core/internal/discord/handlers.go`'s free-text-in-thread handler
   detect a pending question and tag a reply `type:"answer"` with the
   right `reply_to_seq`, instead of always defaulting to
   `type:"discussion"`. This supersedes ADR-0018's framing of the
   dashboard as the only place a question can be answered.

5. **Raw SDK output is relayed, not pre-filtered — and Discord's relay
   flips from a denylist to an allowlist.** `logSdkMessage` now pushes
   every SDK message type (tool calls, tool results, session-init
   metadata, result summaries), not just assistant text blocks, into the
   same transcript path `PushToolTelemetry` already used — no new RPC.
   Assistant *text* keeps `type:"discussion"` specifically, so it still
   reaches Discord exactly as before; everything else is tagged with its
   own raw type. `core/internal/transcript/relay.go`'s auto-skip flips
   from a denylist of exactly one type (`tool_call`) to an explicit
   allowlist (`discussion`/`approve`/`abort`/`question`/`answer`/empty) —
   a denylist would forward *any* new type nobody remembered to exclude
   straight to Discord by default, and raw tool output (Bash stdout/
   stderr, file contents) reaching a Discord channel verbatim is a real
   secret-leak risk, not a hypothetical one, now that every message type
   flows through this path. The transcript itself, and the dashboard's
   rendering of it, stay unfiltered regardless — only what reaches
   Discord is gated.

6. **The wrapper's own responsibility shrinks to bootstrap, heartbeat,
   and status reporting.** `pushAndOpenPr`/the wrapper-owned push step
   are deleted — the agent runs `git commit`, `git push`, and `gh pr
   create` itself, via Bash, inside the session, whenever it decides
   it's ready. `configureGitAuth` moves to run once, unconditionally,
   before the session starts (it used to run lazily, immediately before
   the wrapper's own now-deleted push call) — the agent needs working
   git/`gh` auth whenever it gets around to pushing, not just at one
   fixed point the wrapper used to control. Since the wrapper no longer
   constructs or directly observes the PR itself, it gains a
   post-session `verifyPrExists` check (`gh pr list --head <branch>
   --json url`) before reporting the task `done` — confirming a PR
   genuinely exists rather than trusting the agent's own `PR_READY:`
   summary at face value. **No wrapper-side lease-check-before-push was
   added** to replace the one this design removes — considered and
   rejected: agent-dependent safety isn't safety, and bundling the check
   into one atomic "ship it" tool would still be guarding from inside
   the pod, the wrong layer either way. The residual risk (a stale,
   already-reclaimed pod pushing anyway) is accepted, covered by
   ADR-0024's faster crash detection shrinking the window it could
   happen in — the remaining fallback is a human closing a duplicate PR,
   not a lock.

## Alternatives considered

There was no serious competing option for the core "delete the phase
machinery, don't harden it" call — once ADR-0021 established the real
streaming-input session, the fleet-orchestrated protocol on top of it was
solving a problem Claude Code already solves natively. The one real
alternative debated was point 6's wrapper-owned push step: keep a
lease-check tool the agent calls before pushing, as an explicit safety
gate. Rejected as above — a check the agent itself decides whether to
invoke isn't a safety property, and moving it doesn't change what layer
it runs at.

## Consequences

- `worker/src/planning.ts` and `worker/src/index.ts` both shrink
  substantially — no phase-tracking state (`approved` remains, as the
  one flag `canUseTool` reads; everything else — `planCount`,
  `awaitingCheckpointReply`, `MAX_PLANNING_ROUNDS` — is gone), no
  wrapper-owned git push/PR logic.
- "Phase" disappears from the code entirely — it's still a reasonable way
  to *narrate* what a task is doing (a human reading the transcript will
  see explore/plan/implement happen in roughly that order), but nothing
  in the fleet enforces or gates on it as a state anymore.
- Discord and the dashboard become uniformly thin transports over the
  same underlying message stream, gated only by the relay allowlist —
  not by which fleet-defined phase a task happens to be in.
- **ADR-0005 is further revised** (its index entry already noted ADR-0021
  corrected the *enforcement* mechanism without changing behavior; this
  ADR changes the *trigger* — see this file's point 3).
- **ADR-0008's round-cap description is superseded outright**, not just
  corrected a second time — the checkpoint mechanism ADR-0021 had already
  described as "in-session pause, not teardown" is now deleted, since the
  phase boundary it existed to pause at no longer exists.
- **ADR-0018 is superseded** — its "no correlation ID needed" argument
  was the actual gap this ADR's point 4 closes, and Discord can now
  answer a question, contradicting ADR-0018's dashboard-only framing.
- **ADR-0021's core verified mechanism is unchanged and remains the
  current foundation**: one continuous streaming-input session,
  `Write`/`Edit` absent from `allowedTools`, `canUseTool` as the live
  gate, `interrupt()`/`setPermissionMode()` as the SDK's own control
  primitives. This ADR builds directly on top of it — it does not
  revisit or weaken any of it.
- `BASE_BRANCH` (per-repo, e.g. `vos-monolith` uses `dev`) now threads
  into the merged `taskPrompt` directly, since the agent constructs its
  own `gh pr create --base` call — previously this only had to reach the
  wrapper's own `pushAndOpenPr`.
