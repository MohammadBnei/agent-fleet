# ADR-0058 — An answer wakes the session, and a permission's pod may not be reaped

- **Status:** Accepted
- **Date:** 2026-08-19
- **Amends:** [ADR-0050](0050-a-question-outlives-its-pod.md) — its delivery half was never built. The decision stands; this is the code that makes it true.
- **Related:** [ADR-0048](0048-one-session-one-pod-one-shared-home.md) (warm-then-append), [ADR-0013](0013-go-fleet-core-and-e2e-provisioner-rewrite.md) (pull/cursor, idempotency keys), [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) (`canUseTool` blocks), [ADR-0040](0040-session-liveness-and-stall-guard.md) (the sweeps)

## Context

Reported live on 2026-08-19: *"a message said I had not responded to a question
in time, and now the pod is dead"*, then *"new questions don't show either"*.

From core's own logs, session `8e5b57c0` (`editable-blog`), on 4.10.0:

```
18:47  discord: notified blocked session            (editable-blog)
19:13  PromptSession REJECTED — "blocked waiting on a human decision"
19:46  sessions loop: idle timeout, tearing down pod   after=1800000000000
20:40  mcp AskUserQuestion  questions=4  timeoutMs=120000
20:41  ERROR mcp AskUserQuestion  "code = Canceled"      ← exactly 60s later
```

At 20:41:57 the agent gave up on the tool and pasted its four questions into the
chat as prose: *"Questions live on dashboard. If they not render, answer here."*

Four independent defects, none of which CI could have caught, and none of which
is visible from the outside — the transcript row is written either way, the
badge clears either way, the console looks plausible either way.

**ADR-0050 already decided the behaviour that was wanted.** A question returns
`pending`, the turn ends, the pod is free to die, and the answer is delivered to
the next pod on warm. What it did not have was an implementation of the last
clause.

## The invariant

Stated once, because three of the four fixes fall out of it and the codebase
already half-said it in `ReserveSlot` and ADR-0050 §2:

> **A question is durable and pod-independent. A permission is bound to one
> live pod.**

A question's answer is a transcript row keyed by the question's own seq,
readable by whichever pod comes next. A permission's allow/deny is bound to a
specific pod's live `canUseTool` promise; once that pod is gone the buttons
reach nothing.

An architecture review of the first draft of this change caught it applying the
invariant correctly in one place and contradicting it in two others. Both are
corrected here, and the wrong version is recorded below rather than edited out.

## Decision

### 1. `AnswerQuestion` warms before it appends

`ReadSince` is `seq >= sinceSeq`; `LatestSeq` is `MAX(seq)+1`. `resumeFromSeq`
is read inside `WarmIfIdle`, so **an entry appended to a cold session lands one
below the cursor of the pod that warming is about to create** — and the next
warm's cursor will be above it too.

An entry appended to a cold session is therefore not "delivered later". It is
**undeliverable forever**. ADR-0050's central claim was false as implemented for
every answer that landed before a warm, which is every answer to a question
whose pod had already been reaped — the case the ADR was written for.

`AnswerQuestion` now calls `WarmIfIdle` first, exactly as `PostMessage`,
`OpenFromProposal` and `PromptSession` do. The worker's receiving branch has
existed since 0050 and had never once run.

Two accepted consequences: warming runs `ReserveSlot`, so answering a question
stale-denies any permission the dead pod left behind (it was already
unanswerable, but it is an auto-decision triggered by answering something else);
and this RPC can now fail with `ResourceExhausted` or `FailedPrecondition`,
which is honest — the answer had nowhere to go in those cases before either, it
just failed silently.

**`RespondToPermission` deliberately does not warm.** `ReserveSlot` stale-closes
unanswered `permission_request` rows on every warm, so warming there would
auto-deny the request being answered, a millisecond before recording the human's
real decision.

### 2. The idle sweep exempts permissions, not questions

`ListIdleSessionIDs` reaped any live pod with a stale `last_active_at`. Asking
moves that timestamp once and then, by construction, nothing else happens until
a human answers — so the fleet tore down sessions for waiting on the human they
were waiting on.

**The first version of this fix exempted every pending decision, and was
wrong.** It contradicts ADR-0050 (*"the pod is free to die"*), and with
`MAX_IN_FLIGHT_TASKS = 5` one forgotten subsidiary question would have held 20%
of the fleet indefinitely. It is also self-defeating: decision 1 exists
*because* the pod dies.

The sweep now excludes sessions holding an unanswered **`permission_request`**
only. Reaping that pod destroys the agent's whole turn and orphans the human's
click; holding a slot is the cheaper loss. A question's pod is expendable.

### 3. `AskUserQuestion`'s `timeoutMs` argument is deleted

The 60s ceiling is `DEFAULT_REQUEST_TIMEOUT_MSEC` in the agent's own MCP client
— one hop from the sidecar and invisible from it. The agent asked for 120000
because the schema invited it (*"Call the tool again with the same questions to
keep waiting"*), contradicting both the tool description twenty lines up and
ADR-0050 §3, which say end your turn instead.

There is nothing an agent could know that makes one value better than another;
the only requirement is "comfortably under a deadline it cannot see". So the
knob is gone rather than clamped, and the wait is a fixed 45s — not 60s, which
would race the client's own deadline.

A `Canceled`/`DeadlineExceeded` from core now returns the designed
`{"status":"pending"}` body instead of a tool error. The question row is durable
either way, so "we stopped waiting" and "the tool failed" are different claims,
and reporting the second is what made an agent abandon the tool. Narrow on
purpose: core being unreachable still surfaces as a real error.

### 3b. A re-ask supersedes the question it replaces

Decision 3 stops the retries. This makes them survivable if anything ever
retries again, and it is the defect that actually made questions unanswerable.

`reuseOrAppendQuestion` compared question text **byte-for-byte**, and the model
regenerates that JSON on every call, so a re-ask differing by a word or an
option order missed the reuse and appended. Measured live on session
`b7753602`: two calls 72 seconds apart, three questions each, **both announced
blocked** — `announceBlocked` fires only on a new append, so the reuse added for
exactly this had never matched once.

Accumulating is what kills the feature. `pending_decisions` counts every
unanswered question, so a session gathering one row per retry can never be
unblocked by answering: the human answers the card they see, the count stays
above zero, and a fresh card replaces it. Reported as *"the question is dead — I
can't even respond."*

An agent can only ever be blocked on one `AskUserQuestion` at a time — its own
tool call blocks synchronously, ADR-0018's founding assumption and still true —
so an older unanswered question is by definition a dead retry, not a second
thing being asked. A new question now closes the ones it replaces.

Closed *before* the new row is appended, because the dashboard looks for a
question with no **later** answer of any kind. Authored by `agent`, which keeps
it out of both delivery paths: core's poll matches only `From == "human"`, and
the worker's stream handler skips anything not from a human or a peer session.

### 4. The console fetches the newest transcript page, and gates decisions by kind

The session list called `getTranscript({ sessionId, sinceSeq: 0n })` with no
limit, and `transcriptWindow` treats a zero limit as a forward read — so it got
the **oldest 1000 entries**, while the detail view, which always passed a limit,
got the newest 200. Past a thousand entries the list could not see a pending
decision at all, and a *new* question was the most invisible thing of all,
landing at the high end. Core meanwhile counted over the whole table and kept
reporting the session blocked, which is why the card was red and empty.

The list now passes a limit. The server default is unchanged: `sinceSeq: N` with
no limit meaning "forward from my cursor" is a coherent contract that the
`beforeSeq` branch depends on.

Decision UI is gated **per decision kind, not per pod liveness** — a question is
answerable with or without a pod now that answering warms; a stranded permission
renders read-only with the reason. Stated once in `decisionAnswerable` and
applied in both `DecisionInline` and `DecisionDock`, because a rule applied to
one surface makes the other lie. A stranded question is bucketed into NEEDS YOU
rather than STUCK, since it is a live decision again.

### 5. `PromptSession` no longer drops prompts on a capacity rejection

Its comment claimed a best-effort warm was safe because *"the entry is durable
either way and a later warm will pick it up from a cursor computed after it
exists"*. A cursor computed **after** the entry is **above** it. Durability was
never in question; no pod ever read it. It now fails instead — the caller is an
agent with a tool result to read, and "the fleet is full" is a fact it can act
on where a silently discarded prompt is not.

## Consequences

- A question can now be answered from the console at any time, and the session
  wakes to receive it. This is the first time that has been true.
- A permission-blocked session holds its pod until answered. Deliberate: the
  alternative destroys the decision. A forgotten permission consumes a slot, and
  the STUCK section exists to surface that.
- The warm-then-append rule now lives on `WarmIfIdle`'s doc comment, where it is
  a property of the `resumeFromSeq` read rather than of any one caller. Its four
  copies became one-line pointers.
- **Not fixed, named instead:** `resumeFromSeq` truncates every human entry that
  already exists. Decision 1 makes ADR-0050 true for exactly one button. A
  general fix needs a record of what the previous pod consumed, which nothing
  keeps — resuming from the oldest unanswered entry instead would re-deliver
  stale `abort`/`interrupt` rows, the regression `LatestSeq`'s own comment
  exists to prevent. Marked `ponytail:` with the upgrade path (a `cold_from_seq`
  column set on append-while-cold, cleared on warm).

## Verification

Every fix has a test that was **observed failing against the shipped code**
first:

- `TestAnswerQuestion_WarmsFirstSoTheAnswerIsAboveTheResumeCursor`
  (`-tags=integration`) — a recording provisioner captures `resume_from_seq`.
  Against the old ordering it reports *"answer at seq 1 is BELOW the new pod's
  cursor 2"*: the production bug, off by exactly one.
- `TestListIdleSessionIDs_APermissionHoldsThePodButAQuestionDoesNot` — asserts
  the asymmetry in both directions, so exempting too much fails as loudly as
  exempting too little.
- `TestAskUserQuestion_ACancelledWaitIsPendingNotAnError`, plus one asserting a
  genuine failure still errors and one pinning the wait under 60000.
- `TestAskUserQuestion_ARewordedReAskSupersedesTheOldOne` — against the shipped
  code it reports `pending questions = [0 1], want exactly [1]`, which is the
  accumulation the operator was hitting.
- `TestTranscriptWindow_ALimitedReadReturnsTheNewestEntries` seeds past the
  1000-entry cap, because a short transcript passes either way; a source guard
  pins the client's call shape, since that regresses by *deleting* an argument.
- `decisionAnswerable` and `bucketSessions` tests pin the per-kind rule.

No cluster run before merge, by the operator's call.

## Provenance

Diagnosed on the live cluster over SSH, read-only. The four defects were found
in this order: the idle teardown in core's logs, the 60s cancel in the sidecar's,
the transcript window by reading `transcriptWindow` against its own test, and the
undeliverable answer by tracing `resumeFromSeq` arithmetic after the first three
failed to explain why answering had never helped.

The first draft of this ADR proposed exempting all pending decisions from the
idle sweep and rendering all stranded decisions read-only. A systemic review
rejected both against ADR-0050 before any of it shipped. The reviewer also found
decision 5, which none of the reported symptoms pointed at.

Decision 3b was missed entirely on the first pass and found only because the
operator pushed back — *"are you sure you found the root cause?"* — after the
other four were already committed. The timeout error had been read as a
nuisance; nobody traced what the retry it caused actually **did**. The evidence
was three `discord: notified blocked` lines in a log already quoted in this ADR:
that message fires only on a new append, so the retries had been visibly
appending all along.
