# ADR-0078 — Core on the bridge: its sign-in, session announcements, the dispatcher's rules, and what agents cannot touch

- **Status:** Proposed; §5's closing and §8's echo are amended by [ADR-0079](0079-provenance-ledger.md) (proposed)
- **Date:** 2026-10-23
- **Amends:** [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §2–4 and §6 (how core signs in, which types
  it reads, how the dispatcher calls `Open`, what a proposal carries); infra-bootstrap ADR-0066 §3–4 on three points
  recorded there: `owns` may leave its types empty, a match-all subject is accepted (only globs aimed at `fleet/` or
  `game/` are refused), `position` is the transcript `seq`, and the last published edge is stored and retried
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
   - **Configuration** is an optional mounted file, re-read before every attempt: key `app_password` of the Secret
     `bridge-core` in `agent-fleet`, delivered by infra-bootstrap.
     - absent, or holding the literal `<no value>`, means the bridge is not configured: one Info line naming the
       reason, and no dispatcher;
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
     - One reconcile function (`core/internal/announce`) computes the count. It is nudged after every append that asks
       or answers (the transcript decorator) and runs over every opted-in session on a 30-second tick, which catches
       `ReserveSlot`'s stale close and any other path that bypasses the decorator.
   - **Data:** `{thread_id, pending, reasons, position}`.
     - `reasons` lists `question`, `permission` and `plan`.
     - `position` is the transcript `seq` of the entry that changed the count. It is durable and grows per session.
     - Never the definition, the opener, a prompt, a tool input or any transcript text.
   - **Envelope:** `actor=machine`, `actor_login=core`, `trusted=false`, no `owner`, no `causation_id`. The CloudEvent
     id is `<thread id>:<position>:<edge>` (`waiting` or `resumed`): a session archived while waiting resumes at the
     position of its wait, and the bridge dedups on the id.
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
     - a subject glob aimed at their threads: one that can match some subject starting with `fleet/` or `game/`.

     The types are the fence. A match-all subject (only `*`, or no subjects) is accepted, because only reserved types are
     published on those threads. An `owns` may still leave its types empty, because it routes by repo or component.
     Core's own bridge subscription never carries the reserved types (infra-bootstrap ADR-0066 §3), and the bridge
     enforces that.
   - **`PublishEvent` refusals.** It refuses a reserved type or subject, so an agent cannot announce a wait for
     someone else's session.

5. **The dispatcher's rules** (ADR-0071 §3–6, as built in `core/internal/dispatch`):
   - **Subscription:** `bridge-core/dispatch`, types `dev.bnei.github.`, `dev.bnei.alert.` and `dev.bnei.agent.`, no
     subject filter. Nothing is read until the definitions registry has synced and lists the client `bridge`: routing
     against an empty registry would ack every event as unowned.
   - **Routing.** A definition receives an event when its `owns` or its `subscribes` matches and `MayOpen(bridge, it)`
     holds. In `owns`, every list the definition fills must match (repos against the subject's
     `github/<owner>/<repo>/…`, by name or `owner/repo`; subject globs; type prefixes), and at least one is filled. No
     event names a component yet, so an `owns` listing components matches nothing. Alerts are never routed to
     definitions (§6).
   - **Idempotency key:** `<source>:<id>:<definition>`, one per target definition, so fan-out never collides with
     itself. The causation id is `<source>:<id>`.
   - **Gate decisions are stored.** `dispatch_decisions(source, event_id, definition, wake, tainted, outcome)` is
     written before the call and pruned after 31 days, past the bridge's retention. A redelivery reuses the stored
     decision instead of recomputing it from state that has moved on, and one with an outcome does nothing.
   - **The gate.** `wake` needs a trusted event, never a replay, from a person (`human`) or a hand-off through
     `PublishEvent` (`agent`, login `agent:<definition>`), whose login is in `bridge`'s `actors.allow`, on a thread not
     waiting on a person (an unanswered request while its pod is live; one left by a pod that is gone is closed as
     stale at the next warm). `tainted` is `!trusted`. Anything else is delivered `wake=false`: `context_only` on an
     open thread, a proposal otherwise.
   - **Typed errors, and what each one does:**
     - `Open`'s "no open thread, not waking" is the exported `agentsvc.ErrNeedsWake` (`errors.Is` sees it through the
       connect error): a proposal, then ack;
     - `ResourceExhausted`, from the client's thread cap or from the warm: a proposal, then ack, because the bridge is
       head-of-line;
     - `PermissionDenied`, a stored wake whose actor has since left `actors.allow`: a proposal, then ack;
     - `Aborted` (the thread's lock is held, say by an admin's open warming a pod, or every `Open` slot is taken):
       retried every 2 seconds for up to 2 minutes, then left unsettled, never nacked, since five Nacks dead-letter
       within about 15 seconds. The bridge redelivers it on the next stream;
     - anything else from `Open` (a stale spec, an inactive repo, the provisioner): Nack, so the bridge retries
       and, after its attempts, dead-letters it where the operator sees it.
   - **Over capacity.** The pools are checked only inside the warm (`ReserveSlot`), after `CreateThread`, so a
     pre-check would race. Instead, `Open` archives a thread it created in the same call when the warm fails, for
     capacity or any other reason, which frees the key: later events never land in a session that never ran. An
     over-cap event on an existing thread becomes a proposal too: opening it delivers the message to that thread.
   - **No owner.** An event no definition owns or subscribes to (after `MayOpen`) is acked and logged at debug,
     never turned into a proposal.
   - **Event proposals keep their context.** A proposal filed for an event carries its `definition`, `thread_key`,
     `causation_id` and taint (migration 000020; the source gains `event`). There is one per thread while it stands
     (`dedup_key` `event:<definition>:<thread>`): a later event of that thread folds into its body, once per event and
     up to 60 KiB, and taints it (and its title) if untrusted. Once opened, its key re-arms the way a schedule's does, when its
     session's pod is gone. Opening one is the clicking admin's own act, ADR-0048 §1's human gate: core opens the
     definition's thread through `Open`'s in-process path as client `bridge`, waking, with the proposal's taint: a
     tainted proposal wakes an existing thread too, still marked untrusted, since the click is the gate. The actor is
     the admin, recorded as `admin:<authentik sub>`, which skips the `actors.allow` check. The idempotency key is
     `proposal:<proposal id>:<hash of body and admin>`: the dispatcher's own key already holds a claim with another
     actor, and the hash makes a retry after more events folded in, or another admin's click after a failed attempt,
     a new request rather than a refused one. The proposal is marked opened only while its body is the one delivered;
     an event folded in meanwhile is delivered again with the rest. It becomes the definition's thread with its
     profile, not a plain repo session.
   - **Closing.** A PR closed or merged calls `CloseThread` for each target definition and dismisses that thread's
     unopened proposal. An alert never opens a thread (it is `machine`), so a resolved alert dismisses its proposal
     if nobody opened it.
   - **Gaps.** Core has no GitHub client to resync from. On a gap message it logs a WARN with the range and the count of
     open event threads, then `ConfirmGap`. A thread whose closing event was lost stays open until someone closes it.
   - **Replays never wake:** an event marked `replayed` is appended `context_only` at most.
   - **Messages.** An event's message is its envelope plus what a person wrote in it (a PR's title and description
     when opened, a comment, a review), quoted up to 16 KiB with every line indented (`> `), so it cannot pass for
     an envelope line, and never the whole webhook payload.

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

8. **`PublishEvent` and the `publish_event` tool** (ADR-0071 §2, as built in `core/internal/dispatch/publish.go`):
   - **Who:** a definition session whose pinned spec lists `publish_event` in `sidecarTools`. The tool is registered in
     every sidecar, so core's handler checks the grant; a repo session, an archived one, or a definition without the
     grant gets `PermissionDenied`, and so does the provisioner token. The tool joins `sidecarCeiling`; no definition
     grants it yet. The worker sends it through `canUseTool`, which asks in `plan` mode, like `prompt_agent`.
   - **What the agent sets:** a type under `dev.bnei.agent.` (lowercase letters, digits, `.`, `_`, `-`), a subject on
     a `github/` or `agent/` thread with no whitespace or control characters (both become lines of the receiver's
     message), text up to 16 KiB, and an optional idempotency key. A reused key with another type, subject or text is
     refused, never dropped by the bridge's dedup. The data is `{text, definition}`, quoted in the receiver's message.
   - **What core sets:** `actor=agent`, `actor_login=agent:<definition>`, and `causation_id`, the bridge event
     (`<source>:<id>`) of the session's latest waking entry written by the dispatcher (author `client:bridge`): any
     other client may put what it likes in `causation_id`, so its entries vouch for nothing. The dispatcher stores
     each delivered event's hops with its decision (migration 000021), so `hops` is that event's plus one.
   - **Trust:** the causing event's (its stored taint), false once the session has any context-only or untrusted entry,
     and false when no event caused the session, since nothing vouches for it.
     *Amended by ADR-0082 §3 (built in `000023`):* only an untrusted entry makes it false, not a context-only one,
     and a stored taint is also set for a trusted actor the bridge may not claim (a bot, the fleet's GitHub login).
   - **Bounds:** refused past `maxPromptDepth` (3 hops), which bounds a chain's length; a session publishes at most 10
     events an hour (`published_events`), which bounds its width. The dispatcher caps the wakes `PublishEvent` causes
     at 3 per thread and definition per 24 hours, since hops do not survive a GitHub round-trip; past that, a hand-off
     arrives as context.
   - **The fleet's own echo taints.** A definition that comments or pushes on its PR as the fleet's GitHub identity
     gets that event back as `actor=agent`, untrusted, appended `context_only` to its own thread, which taints the
     session: its later hand-offs are untrusted and never wake. [ADR-0079](0079-provenance-ledger.md) proposes the
     answer: writes through core are recorded, and their echoes carry the writer's trust.

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
- **New tables:** `session_announcements` (000019) and `dispatch_decisions` (000020, with thread, hops and agent
  columns in 000021), plus proposal columns (000020).
- **The alert handler** is shared by both paths. The direct path keeps dropping `resolved`; the bridge path dismisses
  an unopened proposal on it.
- **Core depends on the bridge module's generated client** (`github.com/MohammadBnei/ukubi-bridge`, a pseudo-version
  of its main).

## Rollout

1. This ADR, the loader refusals and `announceSessions`, which the loader refuses until step 2 publishes them (no
   bridge needed).
2. The announcements, with the reconcile and its table.
3. The alert rule on the direct webhook (useful alone).
4. The bridge client and the dispatcher.
5. `PublishEvent` and the tool.
6. Live, once the cluster is up: infra-bootstrap delivers the `bridge-core` Secret, and a PR event opens
   a definition thread end to end.
