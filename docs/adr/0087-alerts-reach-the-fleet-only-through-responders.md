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
- Today `alert-responder` always fails with `failed_precondition: definition sessions never run on a cluster-access
  repo`, because `THOT_REPO` points at infra-bootstrap. The owner is deciding between two fixes: point the responder
  at a repo without cluster access, or allow definitions on one.
- The failure is loud: an error log line, the counter, and an alert rule on it
  (`increase(agentfleet_responder_start_failed_total[15m]) > 0`, warning, in infra-bootstrap's Prometheus rules).
- The unset env vars may still be set in infra. Core ignores unknown env vars, so nothing breaks. Removing them is a
  follow-up in infra-bootstrap.
