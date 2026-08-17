# ADR-0041 — One session can prompt another, and a pod can prove it is still the pod

**Status:** Accepted
**Date:** 2026-08-12
**Related:** `0020` (hub-and-spoke; core commands, provisioner executes),
`0029` (a permission decision is always a real human decision), `0040`
(session liveness), `0013` (pull/cursor reads, never a bare watch).

## Context

`0040` gave sessions a liveness state. That makes a question expressible
that previously wasn't: *is the other session busy, finished, or genuinely
blocked on a human?* [herdr](https://github.com/herdrdev/herdr) uses
exactly that to let agents coordinate — `agent prompt <name>` delivers a
message to another agent, and `agent wait --until blocked` parks until that
agent actually needs a person. The fleet had no equivalent: sessions were
mutually invisible.

Second, a gap deferred twice and now load-bearing: **nothing verified that
a pod is still the pod.** `worker/src/session.ts` took the first
`session_id` the SDK returned and overwrote `tasks.session_id` with it,
without comparing to `RESUME_SESSION_ID` — so a resume that silently failed
(new session, no history, no context) was indistinguishable from one that
worked, while the task row, pod and transcript all still said "resumed".
And `SaveSessionId` was keyed by `task_id` alone, so a torn-down pod still
finishing its shutdown could overwrite the identity of the pod that
replaced it. Tolerable while only humans wrote into sessions. Not once
agents can.

## Decision

### Routing: unchanged, deliberately

**agent → its own sidecar (MCP) → core (gRPC) → target session.** No
worker-to-worker link, no worker-to-provisioner link, no inter-pod MCP
(`0020` points 4 and 6). An agent never learns another pod's address; the
sidecar injects the caller's own task id rather than accepting it as an
argument, so a prompt cannot claim to come from a session it isn't.

Three MCP tools, registered statically for the same reason `run_command` is
(`0039`) — a resumed session must have them from turn one:
`list_sessions`, `prompt_agent`, `wait_for_agent`.

### Addressing by task id, not a name registry

herdr needs short unique live names because panes are anonymous. Tasks
already have stable ids, and `list_sessions` makes them discoverable. A
name registry would be a second identity space to keep in sync with no
question it answers better.

### The guards are the feature

| Guard | Why |
|---|---|
| **Delivery is always a `discussion` entry from `session`** | An `answer` or `permission_response` from this path would resolve a human's decision on their behalf, quietly voiding `0029`. Structurally impossible, not merely unused. |
| **Refuse a `blocked` target** | It is waiting on a *human*. Pushing a turn into it invites the caller to believe it had answered. |
| **Refuse self-prompt** | A session's own messages already reach it as its next input — this is a self-feeding loop, not a no-op. |
| **Depth cap (3), set by the sidecar, never the agent** | A→B→C→A is a livelock with no human in it. A caller that could choose its own depth could choose 0 forever. |
| **Refuse an unapproved `proposed` task** | Same reason `WarmIfIdle` guards it: a machine-created task no human approved must not get a pod, and prompting warms one. |

Warming an idle target reuses `dashboard.Server.WarmIfIdle` (exported, and
injected into `coreserver` via `SetWarmFunc`) rather than a second copy.
That function carries the capacity cap and the `proposed`/`pending` gates;
two implementations of pod dispatch is precisely the drift `0020` point 2
exists to prevent.

### Prompts are authored `session`, not `agent`

A worker relays its own output as `from: "agent"`, and the human-message
stream filters those out precisely so a session cannot feed itself in a
loop. Delivering prompts as `"agent"` therefore never reached a live target
at all — the entry landed in the transcript and the running agent never saw
it. Found on a real cluster, not by any test: every unit and integration
test asserted the entry was *written*, which it was.

`"session"` is deliverable (core's stream and the worker both accept
`human` or `session`) and still unmistakably not the target's own voice.
It also counts as owing a reply for `0040`'s stall derivation, exactly as a
human message does.

### `wait_for_agent` polls, and a timeout is an answer

Polls Postgres every 2 s rather than subscribing, for `0013`'s reason: a
missed event on a bare watch is unrecoverable, and a poll cannot miss one.
A timeout returns `timedOut` with the state actually reached, not an error
— "still working after two minutes" is a legitimate result to act on. A
target with no live pod returns immediately: nothing is running to change
its state, so waiting the full timeout is just a slow way to say the same
thing.

`after_seq` fixes a race the first live run walked straight into: waiting
right after prompting an already-idle target returned instantly with the
state it held *before* the prompt landed, because that state was already in
the settled set. A settled state now only counts once the target has
produced activity newer than the baseline — herdr avoids the same race by
requiring an observed change off a pre-submission baseline. The sidecar
remembers the seq of its last prompt per target and fills it in, so the
agent gets this without bookkeeping.

### Identity, folded in

- **Resume mismatch is reported.** If `RESUME_SESSION_ID` was set and the
  SDK hands back a different id, the worker logs it and posts an
  `sdk: "resume_failed"` signal entry. The run continues with the new
  session — a working fresh session beats refusing to start — but a session
  that quietly forgot everything is now visible instead of merely
  confusing.
- **`SaveSessionId` is lease-scoped.** The write carries the pod's
  `LEASE_ID` and is rejected if it no longer matches; core logs the
  rejection. A stale pod losing this race is the correct outcome, not an
  error in the run that won. An empty lease keeps the old behaviour, so a
  worker image predating the field still works.

## Consequences

- Agents can hand off work across repos without a human relaying it, and
  can park until another session genuinely needs a person.
- The blast radius of a confused agent is bounded by the guards, not by
  hoping: it cannot answer a permission prompt, loop a relay, or talk to
  itself.
- `coreserver` now depends on `dashboard` for `WarmIfIdle`. Slightly odd
  layering, accepted over duplicating dispatch. If a third caller appears,
  lift the warm path into its own package rather than adding a second
  injection point.
- Not addressed: **ACLs between sessions.** Every session in the fleet is
  one trust domain today — same operator, same cluster, same credentials —
  so any session may prompt any other. If the fleet ever runs work for
  mutually distrusting parties, this is the decision to revisit first.
- `prompt_agent` has no rate limit. The depth cap bounds chains, not
  volume; a session in a loop of its own making could prompt one target
  repeatedly. Left out deliberately — no observed need, and the wrong limit
  is worse than none — but it is the obvious next guard if it bites.

## Amendment (2026-08-17): delivery, not just the row

Writing the row is not delivering the message. This ADR's own
"Prompts are authored `session`" section already records that lesson once —
every test asserted the entry was written, which it was, and delivery was
broken on the real cluster. The same gap survived one layer further in.

**Observed:** a session used `prompt_agent` to ask another one a set of
questions. The target wrote a long, correct answer — as ordinary assistant
output — and it reached nobody. The caller waited on silence.

The cause is that `core` encodes the sender as prose (`[from session <id>]`,
`interagent.go`) and nothing downstream ever read `entry.from`. The receiving
worker's `onEntry` filter admits `"session"` and then dispatches purely on
`entry.type`, so a peer prompt reached `input.push(entry.text)` **byte-identically
to a human message**. The model had no way to know its output does not reach the
sender, and the id in the prefix reads as display attribution rather than a
routable address.

Three changes, all in the receiver:

- **The worker wraps a peer turn.** The prefix is stripped and replaced by an
  explicit statement of mechanism: this is another agent, that session cannot
  see this transcript, and the way back is `prompt_agent` with this id —
  including that a refusal (the `blocked`-target guard below) means the answer
  was *not* delivered. Deliberately no "reply only if needed" hedge: the failure
  was never silence, it was a full answer written to the wrong place.
- **A peer can no longer resolve a human's permission.** The worker's
  "a plain reply denies what's pending" sweep is now gated on `from === "human"`.
  Guard row 2 of the table above — refuse a `blocked` target — does not close
  this: the blocked-ness check and the append sit either side of a warm plus the
  message stream's own poll, so a permission raised in that window arrived with
  requests pending, and the sweep wrote a `permission_response` on a human's
  behalf, attributed to Mohammad. That row bounds who may *send* to a blocked
  session; it never bounded what a delivered message may *resolve*.
- **`fleet-shared/CLAUDE.md` states the contract.** It is the only
  always-in-context channel (`settingSources: ["user", "project"]`; the worker
  sets no `systemPrompt`), and it said nothing about peer replies. The
  per-message wrapper names *this* caller; the shared context is what makes the
  mechanism known before any message arrives.

The dashboard's cross-session branch is now chosen by the entry's author rather
than by matching the prefix against arbitrary entry text, which also removes a
false positive: a human message beginning with that literal rendered as another
agent's.

**The sender stays prose.** A real `transcript.from_session_id` column was
designed and rejected on cost: it would have had exactly two consumers — the
worker wrapper and the dashboard feed — and both already have the text, since
the prefix has to be kept regardless for `wait_for_messages` (which returns
`{from, text, type}` to an agent) and for new-core/old-worker rollout skew. The
column's real payoff is a consumer that cannot parse text at all: the ACLs and
the rate limit deferred above. That is the trigger to revisit; until then the
duplicated regex (worker + dashboard) is the accepted cost, marked `ponytail:`
at both sites.

**Adjacent, not fixed here:** `SendMessage` validates neither `from` nor `type`,
so an agent can post `from="human", type="abort"` into its own session and
trigger the worker's human-authored abort path. `PromptSession` is not the hole —
it injects the caller id server-side — but the property "these entry types come
from a human" is not true fleet-wide, and no code should assume it.
