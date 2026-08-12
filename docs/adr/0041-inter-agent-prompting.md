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
| **Delivery is always a `discussion` entry from `agent`** | An `answer` or `permission_response` from this path would resolve a human's decision on their behalf, quietly voiding `0029`. Structurally impossible, not merely unused. |
| **Refuse a `blocked` target** | It is waiting on a *human*. Pushing a turn into it invites the caller to believe it had answered. |
| **Refuse self-prompt** | A session's own messages already reach it as its next input — this is a self-feeding loop, not a no-op. |
| **Depth cap (3), set by the sidecar, never the agent** | A→B→C→A is a livelock with no human in it. A caller that could choose its own depth could choose 0 forever. |
| **Refuse an unapproved `proposed` task** | Same reason `WarmIfIdle` guards it: a machine-created task no human approved must not get a pod, and prompting warms one. |

Warming an idle target reuses `dashboard.Server.WarmIfIdle` (exported, and
injected into `coreserver` via `SetWarmFunc`) rather than a second copy.
That function carries the capacity cap and the `proposed`/`pending` gates;
two implementations of pod dispatch is precisely the drift `0020` point 2
exists to prevent.

### `wait_for_agent` polls, and a timeout is an answer

Polls Postgres every 2 s rather than subscribing, for `0013`'s reason: a
missed event on a bare watch is unrecoverable, and a poll cannot miss one.
A timeout returns `timedOut` with the state actually reached, not an error
— "still working after two minutes" is a legitimate result to act on. A
target with no live pod returns immediately: nothing is running to change
its state, so waiting the full timeout is just a slow way to say the same
thing.

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
