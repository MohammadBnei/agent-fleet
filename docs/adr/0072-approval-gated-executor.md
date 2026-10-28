# ADR-0072 — An approval button runs exactly the step it showed

- **Status:** Accepted, not built; §5 amended by [ADR-0082](0082-decision-cards.md): a tainted session cannot propose a
  step, only draft a card, and executor approvals become cards bound to their row
- **Date:** 2026-10-18
- **Related:** ADR-0071 (events, threads, taint), infra-bootstrap ADR-0060
  (`ukubi-bridge`), infra-bootstrap ADR-0055 (`netbird-agent` short-lived JWT),
  infra-bootstrap ADR-0034 (one build runner per build repo),
  [ADR-0035](0035-thot-cluster-agent.md) / [ADR-0037](0037-thot-is-a-worker-task.md)
  (thot and its executor), ADR-0062–0070 (0073: number never used) (defined agents, Agent Vault,
  security phase G0), `executor/cmd/executor/main.go`

## Context

The first event-driven flow (infra-bootstrap ADR-0060) ends with a
zot/buildah agent onboarding a new build repo: registry keys, a build-runner
instance, CI identity claims. Some of those steps are human-only today: a
person runs `ansible-playbook` against real hosts, writes to Infisical, and
edits a PAT in GitHub's UI. The operator wants one surface — the session in
the dashboard — where the agent drives the work to completion and each
human-only step appears as a button. **Approve means the step runs.**

The existing executor is not a model for this. `Exec` runs any argv it is
sent; its only check is a shared bearer token that the worker pod itself
holds (`THOT_AUTH_TOKEN`, read by `kubectlshim`), and an unset token lets
every call through (fixed to fail closed in G0). An agent that can read its
own environment could skip any button.

## Decision

1. **Approval is a row, written by the operator, not a token held by the
   agent.** The agent proposes a step; core writes an `approvals` row with
   the argv, the infra-bootstrap **commit SHA**, the working directory and
   the environment. The dashboard renders it as a DecisionDock button. The
   approve RPC marks the row under the operator's OIDC `sub` — the hashed
   user id, never the email, which can change.
2. **The executor pulls the approved step; nothing pushes argv to it.** It
   authenticates to core with its own identity, fetches the step by id,
   clones that SHA itself, and runs it once. The row is single-use. A
   different argv, SHA, directory or environment is a different row. The
   agent holds no executor credential.
3. **Credentials are minted per step.** HTTPS steps get an Agent Vault
   `av_sess_` token scoped to the step. Agent Vault path rules are a plain
   prefix match with no segment boundary (`/o/repo*` also matches
   `/o/repo-evil`), so a rule names `/o/r/*` and `/o/r.git/*`, never a bare
   prefix. Revoking a token leaves an open tunnel usable for about two
   minutes; the step's timeout must not rely on revocation. SSH steps (ansible over NetBird)
   get an authentik short-lived JWT in the `netbird-agent` pattern
   (infra-bootstrap ADR-0055) — Agent Vault cannot broker SSH.
4. **Steps that cannot be scoped stay human.** The `boundClaims` PATCH on
   the shared CI OIDC identity can only be done by a credential able to
   widen any identity's claims — that is, read any project's secrets. It is
   shown as the exact command plus a "done" button; the agent verifies and
   continues. The build-runner PAT is dropped: the GitHub App mints runner
   registration tokens.
5. **Taint is shown, not blocking.** A step proposed by a tainted session
   (ADR-0071 §7) executes on approval but its button carries a visible
   warning naming the untrusted source. The operator's decision.
6. **Scope.** The only playbook this path may run at first is
   `ansible/playbooks/build-runner-configure.yml`. Widening the list is an
   amendment to this ADR. When the executor ships, the "Fleet-managed
   infra-bootstrap cluster ops" forbidden pattern here and infra-bootstrap
   `CLAUDE.md`'s "a human runs ansible personally" rule are amended in the
   same PR, scoped to this one path. Until then both stand.

## Alternatives considered

- **Approve = "I did it"** (copy the command, run it, click done) — no rule
  change, but the surface stops being one surface for the most frequent
  steps.
- **Approval bound to an argv hash only** — the same argv runs whatever the
  agent left on disk; binding the SHA and cloning it is what pins the code.
- **Reuse thot's executor as-is** — it trusts its caller; see Context.
- **Refuse steps from tainted sessions** — safer, but forces a fresh session
  for any thread that saw an outside comment. Rejected by the operator in
  favour of a visible warning.

## Consequences

- One approval click now runs infrastructure changes. The bound is the
  single-use row, the pinned SHA, per-step credentials and the playbook
  allowlist.
- A tainted-session step can run with one click. Accepted risk, made
  visible.
- Prerequisites: G0's executor fail-closed fix, Agent Vault (ADR-0062–0070; 0073: number never used),
  and ADR-0071's dispatcher.
