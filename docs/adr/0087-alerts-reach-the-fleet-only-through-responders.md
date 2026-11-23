# ADR-0087 — Alerts reach the fleet only through responders

- **Status:** Accepted (owner, 2026-11-23)
- **Date:** 2026-11-23
- **Amends:**
  - [ADR-0086](0086-rule-of-two.md) §3-§4: the line "the alert webhook keeps filing alert proposals" goes. Core files
    no alert proposal at all.
  - [ADR-0078](0078-core-on-the-bridge.md): the duplicate-alerts note and §6 (one proposal per alert, whichever path
    delivers it first). There is one path, and it files no proposal.
- **Supersedes:** [ADR-0037](0037-thot-is-a-worker-task.md)'s alert to proposal step.
- **Related:** infra-bootstrap ADR-0092 (Alertmanager is the sole alert router)

## Context

Alertmanager posts every alert to Discord itself and to ukubi-bridge (`dev.bnei.alert.firing` and `.resolved`).
Core's direct `/webhook/alertmanager` has received nothing since that switch. What was left was a second, unused path
that filed proposals for alerts, plus a fallback that filed one when a responder could not start.

## Decision

1. **An alert reaches the fleet only through a first-responder definition run** (ADR-0086 §3-§4). Its output is an
   `alert_group` card. A run that ends without an answer files the Unanswered card.
2. **An alert no responder subscribes to is acked.** Discord has it and the bridge keeps it. Core files nothing.
3. **A responder run that cannot start files nothing either.** Core logs an error ("the alert reaches no session") and
   increments `agentfleet_responder_start_failed_total{definition}`. The counter starts at 0 for each responder known
   when the dispatcher's stream starts. A responder added later shows up with its first failure.
4. **Core's `/webhook/alertmanager` is deleted**, with `ALERT_WEBHOOK_TOKEN`, `THOT_REPO` and
   `THOT_DISCORD_CHANNEL_ID`, its gate exemption and its network-policy rule. A request to that path now meets the
   gate like any unknown path.
5. **Open alert proposals stay.** The owner closes them.

## Consequences

- Until a responder can run, an alert reaches only Discord and the bridge event log.
- `alert-responder` used to fail with `failed_precondition: definition sessions never run on a cluster-access repo`,
  because its `workspace: { repo: infra-bootstrap }` tripped the cluster-access check (`dashboard/server.go`, create
  and warm). Its workspace is now `agent-fleet`, which fixes that. It still cannot launch until its vault exists.
- The failure is loud: an error log line, the counter, and an alert rule on it
  (`increase(agentfleet_responder_start_failed_total[15m]) > 0`, warning, in infra-bootstrap's Prometheus rules).
  ResponderRunCannotStart is routed to Discord only (infra #558), so a responder that cannot start never answers its
  own failure alert. A core shutdown during the invoke wait is not counted. An alert whose data is not a JSON object
  is counted once per subscribing responder.
- The unset env vars may still be set in infra. Core ignores unknown env vars, so nothing breaks. Removing them is a
  follow-up in infra-bootstrap.

## A responder investigates read-only

*Added 2026-11-23.*

A first responder must look at the cluster before it answers. Without that, its prompt can only answer
`unknown_failure`.

1. **The reads.** `alert-responder` holds `kubectl_read` (`cluster.read`), `view_logs` (Loki, over
   `cluster.logNamespaces`) and `query_metrics` (PromQL through core). All three are tagged B. A responder holds A·B
   (ADR-0086): it holds no C method, though its typed output can select an unattended catalogue action (C, ADR-0086 §1). Core's `AuthorizeKubeRead`, `ViewLogs` and `QueryMetrics` already check
   the stored set for B and the pinned definition's grants. Nothing else changes at run time.
2. **The validator.** `view_logs`, `query_metrics` and `cluster.logNamespaces` were standing-only (ADR-0084 §6). They
   now also validate on a definition with `trust.untrusted: B`. `spawn_worker`, `wait_for_result`, `open_issue`,
   `list_responder_cards` and `speak` stay standing-only. A responder cannot spawn, open issues or speak.
3. **The workspace is `agent-fleet`.** The responder no longer points at infra-bootstrap, a cluster-access repo, so
   the `definition sessions never run on a cluster-access repo` refusal in Consequences above no longer applies. The
   cluster reads come from `cluster.read`, not from the repo. Like `ops-lead`, the pod holds the model credential
   only: no GitHub key.
4. **Accepted risk.** Alert text can steer a responder that now reads B data. It has no free-text output, no write
   tool and egress only to the vault proxy and core. What remains: object names in a card or selection (up to 8 of
   253 characters, DNS-1123) can encode what it read, and `ops-lead` reads those cards; `ops-lead` already holds the
   same reads, so no new B-to-C path opens. Which deployment a catalogue action restarts leaks a few bits per action
   (rate-limited, 6 an hour, one per target per 10 minutes). Query text lands in Prometheus, Loki, API-server and
   core logs. `view_logs` reads every pod of a granted namespace, other sessions' workers included in
   `agent-fleet`, while `extractTaskIDFromMetadata` stays a TODO. `query_metrics` has no namespace scope, as for
   `ops-lead`.
5. **What still blocks a launch** is outside this repo: the vault `alert-responder` in Infisical agent-vault with
   `CLAUDE_CODE_OAUTH_TOKEN`, and the fleet-read ServiceAccount and Roles in infra-bootstrap (#462).

## Responder cards, run end and CI logs

*Added 2026-11-23, from the first live responder QA.*

1. **One voice.** A card names the responder in the third person ("Alert X: the responder could not name the cause")
   and the owner as "you" (options "Investigate" and "Dismiss"). Core writes both. An alertname with a space is plain text and stays;
   one that is not (newline, symbols) reads "An alert", never "Alert alert". A card that names no object or finding
   (`unknown_failure`) appends where the alert fired from the group's own labels (`namespace`, `deployment`, `pod`,
   `job`, each a DNS-1123 name core checked): typed fields only.
2. **An answered alert card ends the run.** A first responder is single-shot (ADR-0086 §3) and ended with its card.
   An `alert_group` card's answer closes the card and stays in the transcript, but warms and resumes nothing (the
   rule ADR-0084 §9 set for a character's card), so no second structured output can be filed.
3. **Titles.** `InvokeRequest.title` (optional) names a run in the session list; the dispatcher sends
   `Alert: <alertname> (<namespace>)`. Empty keeps the idempotency key.
4. **CI job logs.** Forgejo job logs ship to Loki as `{job="forgejo-actions", actions_task=...}` with no `namespace`
   (infra-bootstrap ADR-0090), so `cluster.logNamespaces` can never name them. A definition gets
   `cluster.logJobs`, an allowlist of Loki job labels validated like `logNamespaces` (standing or first responder,
   DNS-1123, a widening refuses a re-pin in place). `view_logs` takes `job` (and `actions_task`, digits) instead of
   `component`/`namespace`; core refuses a job off the list, a job with a namespace, and a job from any session that is
   not a definition session. The selector is built by core from the two checked values: no free LogQL. `alert-responder`
   holds `forgejo-actions`. Risk accepted: job logs hold whatever a CI job printed, B data read by a B-tagged tool.
5. **Card options (2026-11-23).** The old "You handle it" and "Dismiss" both only recorded a handoff to a run that had
   ended. "Investigate" is now an `open_session` effect: core binds a brief of the group's repo (the CI alert's repo
   by name, else `infra-bootstrap`), its title (80 characters at most) and the card's typed findings, never the
   responder's text. "Dismiss" stays a handoff and the card takes the owner's own words as an optional reason; either
   is recorded and resumes nothing (item 2). The console says "Closes the card; nothing runs" for a handoff on an
   `alert_group` card. The responder's prompt lists its four tools; it had tried `journal_search` and
   `set_session_meta`, both denied.
