# ADR-0078 — Core on the bridge: its sign-in, session announcements, the dispatcher's rules, and what agents cannot touch

- **Status:** Proposed
- **Date:** 2026-10-23
- **Amends:** [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §2–4 and §6 (how core signs in, which types
  it reads, how the dispatcher calls `Open`, what a proposal carries); infra-bootstrap ADR-0066 §3–4 on three points
  recorded there: `owns` may leave its types empty, `position` is the transcript `seq`, and the last published edge
  is stored and retried
- **Related:** infra-bootstrap ADR-0060 (the bus) and ADR-0066 (API clients sign in through authentik; reserved
  human-facing types), [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §2 (no queue),
  [ADR-0066](0066-agent-service.md) and [ADR-0067](0067-shared-and-private-sessions.md) (what a client may see),
  `ukubi-bridge` (`internal/auth`, `internal/store` retention)

## Context

ADR-0071 decided how events reach sessions; none of it is built. The bridge it relies on now exists, and
infra-bootstrap ADR-0066 changed three things under it:

- Clients sign in as **authentik service accounts** with 10-minute tokens. Core is `bridge-core`, the only
  privileged client.
- **Reading is bounded too.** A client lists rules pairing type and subject prefixes, and the bridge refuses a
  subscription outside them.
- Two type roots are **human-facing**: `dev.bnei.fleet.session.` (core announces that a session waits on a person)
  and `dev.bnei.game.` (mountain's game). Nothing an agent runs may read or publish them.

mountain's game (its `plan/09-play.md`) is the first consumer of the announcements. It opens sessions as a
registered client and must learn when one of them waits on a player.

A review of the dispatcher's design against the code found that ADR-0071's text, implemented literally, breaks
in several ways:

- **Shared idempotency key.** `Open`'s claims are unique per `(invoker, idempotency_key)`, so fan-out to a second
  definition on the same event id collides with the first.
- **Gate drift.** The gate's inputs (taint, a blocked session, the cap) can change between a delivery and its
  redelivery, so a redelivery can hash differently and file a duplicate.
- **Overloaded `FailedPrecondition`.** It covers seven conditions, of which only "no open thread" means "file a
  proposal".
- **Dead-end proposals.** A proposal opens as a plain repo session in `auto` mode. A definition's event would lose
  its profile, thread and taint.
- **Empty threads.** Hitting the cap after `CreateThread` leaves an empty thread holding the key.
- **Inbox flood.** No definition lists `bridge` today, so treating "no owner" as a proposal would put every
  push and comment on every repo in the inbox.
- **Duplicate alerts.** Alerts now reach core twice: directly at `/webhook/alertmanager` and through the bridge.

## Decision

1. **Core signs in as `bridge-core`.**
   - It runs authentik's `client_credentials` grant with its app password. The grant goes to the in-cluster
     authentik Service, with `Host: authentik.bnei.dev` and `X-Forwarded-Proto: https`, so the issuer is the
     public one.
   - It reuses the token until 10 seconds before expiry (`golang.org/x/oauth2`'s default), under the bridge's
     5-second floor for opening a stream.
   - **Configuration** is an optional mounted file (`bridge-core-secret` from infra-bootstrap), re-read before every
     attempt:
     - absent, or holding the literal `<no value>`, means the bridge is not configured: one Info line, and no
       dispatcher;
     - a failed grant or a lost stream is a WARN with backoff up to 5 minutes, never an error loop.
   - **Core always starts**, whatever happens with the bridge.
   - This is a second identity, never core's console OIDC client (ADR-0056).

2. **Sessions are announced only for clients that opt in.**
   - `AgentClient.spec.announceSessions: true` (default false) makes core publish `waiting` and `resumed` for the
     sessions that client opened (`invoker = <client>`), and for no others.
   - Console sessions and members' sessions are never announced. A client never learns of a session AgentService
     would hide from it (ADR-0066 §5, ADR-0067 §2).

3. **What an announcement is.**
   - **Types and subject:** `dev.bnei.fleet.session.waiting` and `dev.bnei.fleet.session.resumed`, subject
     `fleet/client/<client>/<thread id>`.
   - **Edges of the pending count:**
     - The count is `pending_decisions`: unanswered `question` and `permission_request` entries. A plan to approve is
       a `permission_request` for `ExitPlanMode`.
     - `waiting` is published when the count goes from 0 to above 0, and `resumed` when it returns to 0.
     - One reconcile function computes the count. It is called after every append that asks or answers, after
       `ReserveSlot`'s stale close, and on the sessions loop's tick, which also catches paths that bypass the
       store decorator.
   - **Data:** `{thread_id, pending, reasons, position}`.
     - `reasons` lists `question`, `permission` and `plan`.
     - `position` is the transcript `seq` of the entry that changed the count. It is durable and grows per session.
     - Never the definition, the opener, a prompt, a tool input or any transcript text.
   - **Envelope:** `actor=machine`, `actor_login=core`, `trusted=false`, no `owner`, no `causation_id`. The CloudEvent
     id is `<thread id>:<position>`.
   - **They are hints.** Core keeps per session the last edge it published (`session_announcements`), written only
     after the bridge accepted it. The next tick compares it with the current count and publishes again if they
     differ. It is the session's current state, like `pod_phase`, not a queue of messages (ADR-0048 §2 stands): an
     edge that flipped twice during an outage is published once, as the current state. The consumer applies the
     highest `position` per thread and reconciles from AgentService on start and on a timer.

4. **Nothing an agent runs reads or forges the human-facing types.**
   - **Loader refusals.** The definition loader refuses (the shared helper is `core/internal/events`):
     - a `subscribes` with no `types`, since an empty list matches everything;
     - in `owns` or `subscribes`, a type prefix that is a prefix of, or starts with, a reserved type (so `""` and
       `dev.bnei.` are refused);
     - a subject glob aimed at their threads: one whose literal head starts with `fleet/` or `game/`, or could still
       become one.

     The types are the fence. A match-all subject (`*`, or no subjects) is accepted, because only reserved types are
     published on those threads. An `owns` may still leave its types empty, because it routes by repo or component.
     Core's own bridge subscription never carries the reserved types (infra-bootstrap ADR-0066 §3), and the bridge
     enforces that.
   - **`PublishEvent` refusals.** It refuses a reserved type or subject, so an agent cannot announce a wait for
     someone else's session.

5. **The dispatcher's rules** (ADR-0071 §3–6, as built):
   - **Idempotency key:** `<source>:<id>:<definition>`, one per target definition, so fan-out never collides with
     itself.
   - **Gate decisions are stored.** `dispatch_decisions(source, id, definition, wake, tainted, outcome)` is written
     before the call. A redelivery reuses the stored decision instead of recomputing it from state that has moved
     on.
   - **Typed errors, and what each one does:**
     - `Open` marks "no open thread, not waking" with a typed error detail: a proposal, then ack;
     - `ResourceExhausted`: a proposal, then ack, because the bridge is head-of-line;
     - an untrusted or `machine` event: a proposal, then ack;
     - anything else from `Open` (a stale spec, an inactive repo, the provisioner): Nack, so the bridge retries
       and, after its attempts, dead-letters it where the operator sees it.
   - **Over capacity.** The pools are checked only inside the warm (`ReserveSlot`), after `CreateThread`, so a
     pre-check would race. Instead, `Open` archives a thread it created in the same call when the warm is refused
     for capacity, which frees the key for the event's proposal. An over-cap follow-up on an existing thread is
     appended `context_only` (`wake=false`) and the proposal links that thread.
   - **No owner.** An event no definition owns or subscribes to (after `MayOpen`) is acked and logged at debug,
     never turned into a proposal.
   - **Event proposals keep their context.** A proposal filed for an event carries its `definition`, `thread_key`,
     `causation_id` and taint (migration columns; the source gains `event`). Opening one is the clicking admin's own
     act, ADR-0048 §1's human gate: core opens the definition's thread through `Open`'s in-process path as client
     `bridge`, waking, with the event's taint. The actor is the admin, recorded as `admin:<authentik sub>`, which
     skips the `actors.allow` check. It becomes the definition's thread with its profile, not a plain repo session.
   - **Closing.** A PR closed or merged calls `CloseThread`. An alert never opens a thread (it is `machine`), so a
     resolved alert dismisses its proposal if nobody opened it.
   - **Gaps.** A bridge gap message triggers a reconcile of the open event threads against GitHub, then
     `ConfirmGap`.
   - **Replays never wake:** an event marked `replayed` is appended `context_only` at most.

6. **One proposal per alert, whichever path delivers it first.**
   - Alerts reach core directly (`/webhook/alertmanager`, kept so an outage of the bridge or authentik never
     silences an alert) and through the bridge.
   - Both paths call the existing `proposals.Create` with the alert's fingerprint as `dedup_key`, under the
     configured alert repo and its inactive-repo check. Its partial unique index (`repo, dedup_key` while not
     dismissed) already makes the second delivery a no-op.
   - Only the call that created the proposal pings Discord.
   - Dismissing keeps its meaning, "not now": a still-firing alert is proposed again on its next fire, from either
     path.

7. **Capacity: the existing pools.**
   - Event sessions are client sessions (`owner = client:bridge`), so they draw from `MAX_EXTERNAL_SESSIONS`, inside
     the definition pool, inside the fleet cap.
   - Today that pool is 1, which is stricter than ADR-0071's 3. Raising it is a `k8s/core.yaml` change, and so a
     deploy decision.
   - A share per client (the bridge against the game) is decided when the game's client goes live, not here.

8. **`PublishEvent` and the `publish_event` tool** (ADR-0071 §2):
   - `causation_id` is the bridge event of the session's latest waking entry, and `hops` is one more than that
     event's. Both are stored with the entry when the dispatcher appends it.
   - Trust is the lowest along that chain, and false when the session is tainted.
   - `actor=agent`, `actor_login=agent:<definition>`.
   - It is refused above `maxPromptDepth` (3), and wakes caused by it are capped per thread.
   - The tool joins `sidecarCeiling`. No definition grants it yet.

## Alternatives considered

- **Announce every session to every subscriber.** It leaks console and member sessions to a client that may not see
  them.
- **Derive waiting from `LiveState`.** It reads `none` without a pod, even while a question is pending.
- **One event per decision request.** Parallel tool calls give N waits, and nothing marks resuming. Edges of the
  count give one pair.
- **A table of alert occurrences** keyed by fingerprint and start, to dedup across paths. It breaks "dismiss means not
  now" (a dismissed, still-firing alert would never be proposed again), and the proposal's own dedup already does it.
- **Ignore alerts in the dispatcher until a cutover flag.** The bridge path then does nothing, and cutting over
  loses alerts whenever the bridge or authentik is down.
- **A cap field per client now.** No second client is live, and the pool already bounds it.

## Consequences

- **Hints can be lost or late.** While the bridge or authentik is down, announcements lag, and they are retried
  from the stored edge. Reconciling is the consumer's job.
- **New tables:** `session_announcements` and `dispatch_decisions`, plus proposal columns. All come as new migration
  pairs.
- **The alert handler** gains `startsAt` and thread semantics on the bridge path. The direct path keeps dropping
  `resolved`.
- **Core depends on the bridge module's generated client** (`github.com/MohammadBnei/ukubi-bridge`, a pseudo-version
  of its main).

## Rollout

1. This ADR, the loader refusals and `announceSessions`, which the loader refuses until step 2 publishes them (no
   bridge needed).
2. The announcements, with the reconcile and its table.
3. The alert rule on the direct webhook (useful alone).
4. The bridge client and the dispatcher.
5. `PublishEvent` and the tool.
6. Live, once the cluster is up: infra-bootstrap delivers `bridge-core-secret`, core restarts, and a PR event opens
   a definition thread end to end.
