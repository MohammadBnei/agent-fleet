# ADR-0071 — Events reach sessions through the bridge

- **Status:** Accepted, not built
- **Date:** 2026-10-18
- **Related:** infra-bootstrap ADR-0060 (`ukubi-bridge`, the bus itself),
  [ADR-0041](0041-inter-agent-prompting.md) (`prompt_agent`, which stays),
  [ADR-0048](0048-one-session-one-pod-one-shared-home.md) (§1 human gate, §2 no
  queue), ADR-0062 (the gate amendment: a registered client may open a session),
  ADR-0072 (approval-gated executor), `core/internal/alertwebhook/handler.go`

## Context

Defined agents (ADR-0062–0070 and 0073) are dedicated to one app or one platform
component. To be useful they must hear about that thing without a human
relaying it: a PR to review, an alert, a hand-off from another agent. And
agents must be able to talk to each other asynchronously — one finishes a
review and needs the zot/buildah agent to onboard the registry, without
waiting on it.

Today the only machine-to-session path is `/webhook/alertmanager`: an alert
becomes a *proposal* and a human opens it. `prompt_agent` (ADR-0041) is
synchronous, session-to-session, refused into definition sessions, and
carries a caller-supplied depth.

infra-bootstrap ADR-0060 introduces `ukubi-bridge`, a Postgres-backed
CloudEvents bus that owns ingestion (GitHub App, Alertmanager, Grafana),
the log, retry and dead-lettering. This ADR is core's side of it.

## Decision

1. **Core never holds a queue.** Durable delivery and retry live in the
   bridge. Core handles each delivered event synchronously and returns an
   error to the bridge when it cannot; the bridge retries. ADR-0048 §2
   stands unchanged.
2. **Agents publish through core, never to the bridge.** A `publish_event`
   sidecar tool, granted per definition through `sidecarTools`, calls
   `CoreService.PublishEvent`. Core stamps the verified session (lease), sets
   `actor=agent`, sets `causationid` to the event the session is handling and
   computes `hops` from that chain — the caller cannot set either. Above the
   existing depth limit (3, `maxPromptDepth`) the publish is refused. A
   published event is trusted only if the event that caused it was trusted
   and the publishing session is not tainted: the lowest trust along
   `causationid` (infra-bootstrap ADR-0060 §6).
3. **Core consumes through one dispatcher.** A goroutine subscribes to the
   bridge and calls `AgentService.Open(definition, thread_key, message,
   interaction=ask, lifecycle=resident)` as the registered client
   `definitions/clients/bridge.yaml`. The CloudEvent `source` + `id` (the id is
   unique only per source) is Open's idempotency key, so a redelivery appends
   nothing. Open passes the permission mode
   explicitly — the definition's `spec.mode.default`, never `auto` — so an
   event-opened session does not inherit the `'auto'` column default of
   [ADR-0061](0061-sessions-launch-in-auto.md). Proposals filed for
   untrusted or `actor=machine` events keep ADR-0061's behaviour once a
   human opens them. Every transcript entry an event produces (the opening
   message, a `context_only` follow-up) is appended by core itself: since
   ADR-0064, a lease-authenticated `SendMessage` from a pod accepts only
   `from` agent or worker. Open warms through `WarmIfIdle`, like every other
   warm, so a dispatched definition session gets its context packs
   (ADR-0063). The dispatcher's test asserts `genome_applied=true` on the
   pod it opens.
4. **Routing.** The `owner` is the definition whose `spec.owns` (repos,
   components, subject globs, types) matches the event; until definitions
   exist, an `owner` column on the `repos` table (ADR-0028) does it.
   Definitions whose `spec.subscribes` matches also receive it — fan-out
   for observer agents, such as an always-on Hermes-type agent. Both match
   the way the bridge's own subscription filter does: `types` are prefixes
   of the CloudEvent type (`dev.bnei.github.pull_request`), `subjects` are
   globs with `*` and `?` only (`github/MohammadBnei/*`).
5. **Threads.** The CloudEvent `subject` is the thread key. A session is
   resident per thread: `sessions.thread_key` with a unique partial index on
   `(definition_name, thread_key)` where not archived (ADR-0062's shape; no
   separate table, no status enum). The column ships with the dispatcher, in
   migration 000010 or later: 000008 and 000009 are ADR-0062's and ADR-0064's. The thread closes when the PR is merged
   or closed, or the alert resolves — the alert handler moves to thread
   semantics and stops discarding `resolved`.
6. **The gate** (the ADR-0048 §1 amendment itself is ADR-0062's):
   - A **new thread** opens a session only for a trusted actor — the
     operator and CI — with trust computed by the bridge: a trusted actor on
     a thread the operator owns, from a same-repo head. A push, close or
     reopen counts as whoever did it, not the PR author. The fleet's agents
     and Renovate are not trusted openers: the agents share one GitHub
     identity, and Renovate PR bodies carry third-party release notes.
     Anything else, and every `actor=machine` event (alerts, ArgoCD, zot),
     files a proposal as today. An alert storm still cannot spawn a pod.
   - At most **3** sessions opened by events run at once (the fleet has 5
     warm pods; the operator keeps 2). Over the cap, a proposal.
   - A **follow-up** from a trusted actor wakes the session. Any other
     follow-up is appended `context_only`: quoted, labelled untrusted, read
     on the next wake, never a trigger. A session blocked on a human gets
     follow-ups `context_only` too.
   - All fleet agents push and comment as one GitHub identity (the
     `ukubi-fleet` App's bot, infra-bootstrap ADR-0062), so the bridge tags
     those events `actor=agent` and never trusts them: they never open or
     wake a thread, and arrive `context_only`. Otherwise a session tainted by
     a stranger could act through the shared identity and come back as a
     clean, trusted event. The operator's comments, pushes and reviews on a
     fleet PR stay trusted. A hand-off that should wake another agent goes
     through `PublishEvent`, which carries trust along `causationid`.
   - Wakes caused by `PublishEvent` are capped per thread, because hops do
     not survive a GitHub round-trip.
7. **Taint.** A session that has any `context_only` entry is *tainted*,
   derived from the transcript, not stored. A session opened by an event
   whose causation chain includes an untrusted event starts tainted.
   ADR-0072's approval buttons show it.

## Alternatives considered

- **A GitHub webhook in core** next to the alert webhook — needs a retry
  loop in core (forbidden by ADR-0048 §2) and makes a core path public on
  every host core serves.
- **`prompt_agent` as the wake path** — needs a caller session, refuses
  blocked targets, takes depth from the caller, and is refused into
  definition sessions by design.
- **Auto-wake on every follow-up** — a stranger's comment on a public PR
  would land as a trigger in a session holding registry and runner
  credentials.
- **Every event a proposal** (no auto-open) — keeps the gate but defeats
  the point: PR review would never be automatic.

## Consequences

- Sessions can now be started by events from trusted actors without a
  click. The cap and the trust rules are what bound that.
- `context_only` text still reaches the model on the next wake; it is
  labelled, not neutralised. Taint makes that visible where it matters
  (ADR-0072).
- Core depends on the bridge for every event-driven path; if the bridge is
  down, events wait in it (or at the sender) rather than in core.
