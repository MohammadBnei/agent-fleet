# ADR-0086 — Rule of Two: core sets each session's trust from its trigger

- **Status:** Proposed (owner decisions 2026-11-16, after five doubt rounds on ADR-0084's cause window)
- **Date:** 2026-11-16
- **Would amend:** [ADR-0082](0082-decision-cards.md) §5–§6 (an effect the owner must approve is a proposal, not a
  card); [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) (a proposal carries a brief written by core);
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§4 (no credential in any pod's env); `CLAUDE.md`'s forbidden
  "Fleet-managed `infra-bootstrap` cluster ops" (`CLAUDE.md:231`, `docs/DECISIONS.md:467`), scoped to §3's catalogue
  playbooks run by an executor
- **Related:** [ADR-0084](0084-long-lived-agents.md) (long-lived characters, built on this),
  [ADR-0037](0037-thot-is-a-worker-task.md) (the executor pattern), [ADR-0062](0062-defined-agents.md)

## Context

Agents are stochastic: the same input does not give the same output. That is what lets them diagnose and repair
things nobody scripted, and it is also why nothing an agent says about its own intent can be the security boundary.
Prompt injection is unsolved. A model cannot reliably tell an instruction from data, and every published defense that
relies on the model, or on a classifier in front of it, has been broken by adaptive attacks.

The fleet already has the pieces a deterministic shell needs:
- every effect is a CoreService call that core authorizes per method and per lease (ADR-0057);
- pods hold placeholders, and the vault swaps the real tokens (ADR-0065, ADR-0075);
- core keeps the transcript, so it knows what started each session.

ADR-0084's drafts tried to compute trust per turn from the transcript (the "cause window"), with T0/T1 causes and
cards for everything else. Five doubt rounds found blockers each time: entries merged mid-turn, a pod-written
high-water mark, threads sharing a process, cards answered into a thread that cannot act. The rule was being
re-derived after the fact instead of decided at the start.

The field's current practical answer is to design the capability graph rather than trust the model. For the sources
the owner reviewed, see:
- Meta, *Agents Rule of Two* (ai.meta.com/blog/practical-ai-agent-security);
- Willison, *The lethal trifecta* (simonwillison.net/2025/Jun/16/the-lethal-trifecta);
- Sietsma, *Agentic AI and security* (martinfowler.com/articles/agentic-ai-security.html);
- Beurer-Kellner et al., *Design patterns for securing LLM agents* (arxiv.org/abs/2506.08837);
- the Agent SDK's secure-deployment guide (code.claude.com/docs/en/agent-sdk/secure-deployment).

## Decision

### 1. The rule

In one session an agent holds at most two of:
- **A**, untrusted input: alerts, logs, game guests, public issues and PRs, anything a non-owner can write;
- **B**, sensitive systems or private data: cluster reads, the owner's private repos and notes, secrets-adjacent state;
- **C**, state change or external writes: pushes, issues, remediations, playbooks, speech to the public.

A session that would need all three runs only **supervised** (§4).

### 2. Core decides the set at session start, and it only narrows

- **The trigger decides the set.** Core computes the set when it creates or warms the session, from the tier of the
  entry that starts it:
  - T0: the owner, through the console, an island, or the game's verified owner channel (ADR-0084 §8).
  - T1: a fleet session's `PublishEvent` whose own session was T0 or T1.
  - Untrusted: everything else.
- **The definition gives the ceiling.** It still says which B and C methods the agent may ever hold (ADR-0062). The
  trigger picks within that ceiling:
  - an untrusted trigger gets A plus at most one of B or C (the definition's `trust.untrusted` field says which);
  - a T0 or T1 trigger gets B and C.
- **Enforced by core, not by the prompt.** The set lives on the session row. Each CoreService method is tagged B, C or
  neither, and core refuses a method outside the set (`PermissionDenied`, key `agent.rule_of_two`). The worker's tool
  surface follows from it: the sidecar exposes only the tools whose methods the set admits. No prompt text carries
  the rule.
- **The one-way switch.** A session's set can lose letters mid-session, never gain them. An entry of a lower tier
  arriving in a session (an alert in an owner thread) drops the session's set to what that tier allows, for the rest
  of the session.
- **Visible.** The console and the game cockpit show each session's set (`A·C`, `B·C`, `supervised`) next to the
  agent, and every effect in the feed carries it.

### 3. Self-healing: a catalogue core runs

- **Core owns a remediation catalogue.** Each action is named and typed, and carries a dry-run and a rollback:
  restart a deployment, scale within bounds, roll back an ArgoCD application to its last healthy revision, cordon or
  uncordon a node, re-sync an application.
  - The agent picks an action and its parameters.
  - Core validates the parameters, runs the dry-run, executes, and records the rollback.
- **An A+C session may run catalogue actions without approval.** An alert-triggered ops session can therefore fix
  what the catalogue covers. Injected text can at worst pick a wrong but bounded, reversible action.
- **Playbooks.** Named `infra-bootstrap` Ansible playbooks with fixed variable schemas are catalogue entries that
  always need a proposal (§4).
  - An executor runs them, holding the SSH credentials outside every pod, as `thot-executor` holds the kubectl
    credential (ADR-0037).
  - No SSH key enters an agent pod.
  - This narrows the forbidden "fleet-managed infra-bootstrap ops" line to these named playbooks. Kubespray, pigsty
    and ad-hoc Ansible stay forbidden.
- **Self-improvement of the catalogue goes through the owner.** An action the agent proposes repeatedly is promoted
  into the catalogue by the owner, as a reviewed change in git. Agents never write catalogue files.

### 4. Proposals: what the owner approves

- **An effect outside the session's set becomes a proposal**, reusing the existing `proposals` table
  (`db/migrations/000001_init.up.sql:154`) and `OpenFromProposal` (`core/internal/dashboard/server.go:354`). It shows
  in a list in the console and the game, like schedules and alerts.
- **Core writes the brief.** A proposal carries a brief built from typed fields: the alert name and labels, numbers,
  the agent's findings as structured fields, and links. Raw log lines or alert text never go in. This is a light form
  of the dual-LLM pattern: the brief is data, so injected text does not ride into what the owner reads or into the
  session that follows.
- **Accepting starts a new session** from the owner's T0 acceptance, seeded with the brief.
  - It holds B and C, plus the brief as A, so it is marked **supervised**.
  - It shows in the cockpit with live steer and stop.
  - The owner lets it run or steers it.
  - Its set narrows like any other (§2).
- **Relation to ADR-0082 cards.**
  - Cards stay for decisions inside a session: a permission, a question, a plan.
  - An agent that wants an effect it does not hold no longer drafts a card for it. It files a proposal, through the
    new `CoreService.ProposeAction`, granted per definition like `draft_card`.
  - ADR-0082 §6's card budget and its one open card per subject apply to proposals as well, keyed by proposal subject.

### 5. Memory and skills are code

- **Owner- and fleet-triggered sessions write memory directly.** Writes go through a sidecar tool, not a writable
  mount, as git commits made by the sidecar. Each commit is tagged with the session and its set.
- **Untrusted-triggered sessions write memory only to a quarantine branch** of the same repo. The owner promotes from
  it in one batched review: a daily proposal, or the next owner session's opening summary. Nothing on the quarantine
  branch is read by a session whose set holds B or C.
- **Skills, prompts and pack content** coming from an untrusted-triggered session arrive only as a PR.
- **Agents never write** tier, policy, vault, egress, definition or catalogue files. Those are reviewed config
  (ADR-0062).

### 6. Credentials: no secret in any pod's env

- The vault session token and the core lease leave env for every pod, fleet-wide. Today they are visible to anyone
  who can `get pods`:
  - `LEASE_ID`, `provisioner/internal/k8s/pod.go:485`;
  - the definition worker's `HTTPS_PROXY`, `pod.go:248-251`;
  - the repo sidecar's `FLEET_VAULT_PROXY`, `pod.go:845`.
- The provisioner writes them, per warm, into a per-pod Secret mounted as a file, with no subPath. Env holds paths
  only, and the readers re-read the file per use.
- This is the first PR, before any read role (ADR-0084 §6) exists. It also closes the same exposure to today's
  `thot-executor` role.

### 7. One vault per definition, for every session class (accepted risk)

- An untrusted-triggered session keeps its definition's vault, GitHub write included.
- The owner accepts the allowed-host channel. Anthropic's guide warns that an agent can send data to any allowed
  domain, so an injected A+C session could post to an issue or a gist on the definition's repo.
- **Revisit trigger:** the first public-facing input class beyond the owner's own game (public issues routed to a
  character, guests beyond invitees), or the first incident. The fix is a vault rule set per session class, picked at
  warm.

### 8. What is never the boundary

- A classifier, the model's own resistance, or a prompt. They are layers, never the rule.
- A quiet record. No incident so far is not evidence of safety.
- An agent's judgement of its own permissions. No agent can change its own set, tier, catalogue or vault.

## Alternatives considered

- **Per-turn cause windows** (the ADR-0084 drafts). Trust was computed per turn from transcript entries above a
  pod-written high-water mark. It went through five doubt rounds of blockers. Rejected: the rule belongs at the start,
  where core decides it alone.
- **Pods per trust tier.** One trusted and one untrusted pod per agent. Rejected: the pods shared a vault and a
  definition, so the untrusted one could still act. Cards answered into the untrusted pod could not act. It doubled
  the memory.
- **CaMeL-style taint tracking.** Values are tagged by origin and the tags checked at every effect. It is the
  principled end state and gives provable guarantees at a utility cost. Not now: the provenance plumbing is
  research-grade for one owner.
- **A dual-LLM parser** for every untrusted input. §4's brief written by core is its light form. A full quarantined
  parser waits for a need the brief does not meet.
- **Per-action approval.** People approve nearly everything they are asked, and stealthy attacks pass anyway. Cards
  are kept for decisions; reversible effects are monitored, not approved.

## Consequences

- Trust is one field per session, set once by core, and visible. The cockpit can show what each agent may do.
- Self-healing is real but bounded: catalogue actions run unattended; everything else is a proposal.
- New CoreService surface: the method tags, `ProposeAction`, the catalogue executor, and memory writes through the
  sidecar. ADR-0082 cards stay for decisions inside a session.
- The per-pod Secret file is a fleet-wide change to every Job (golden tests).
- The accepted vault risk stays written here until its trigger fires.

## The bet

That deciding trust once, at the start, from what core knows for certain (who wrote the trigger) bounds a stochastic
agent better than anything it can be told. It also bets that a catalogue of reversible actions covers most of what
self-healing needs, so the owner's attention goes to proposals, not to every step.

## Not doing

- Taint tracking through values.
- A classifier gate as a boundary.
- Agent-editable policy.
- Vault rule sets per session class, for now (§7).
- Free-form remediation from untrusted triggers.
