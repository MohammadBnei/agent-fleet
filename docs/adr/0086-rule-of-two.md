# ADR-0086 — Rule of Two: core sets each session's trust from its trigger

- **Status:** Proposed (owner decisions 2026-11-16)
- **Date:** 2026-11-16
- **Would amend:**
  - [ADR-0082](0082-decision-cards.md):
    - §1: an alert wakes a first-responder definition (§3);
    - §2: the tier table gains alerts, schedules and Invoke (§2);
    - §3's Publish rule: T1 also needs the publisher's set to lack A (§2). §3's "no taint" otherwise **stands**, and
      this ADR adds no read-taint;
    - §6: new card effect verbs (§4);
    - §8: the card budget is split by trigger tier;
    - §9: the merge right excludes protected paths (§5).
  - [ADR-0070](0070-headless-invoke.md) §1: the dispatcher, an in-process principal, may `Invoke` a first-responder
    definition, keyed `(dispatcher, alert:<groupKey>)` (§3).
  - [ADR-0072](0072-approval-gated-executor.md):
    - §6 widens its playbook list to the catalogue's named playbooks;
    - §1, its approval becomes an ADR-0082 card option bound to the row (already so by ADR-0082 §8).
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§4: no credential in any pod's env (§6).
  - [ADR-0037](0037-thot-is-a-worker-task.md) and [ADR-0035](0035-thot-cluster-agent.md), and the `CLAUDE.md` executor
    row (`CLAUDE.md:62`):
    - the mutate verbs leave `thot-executor` for a remediation executor, so `thot-executor` becomes read-only;
    - the remediation executor is a second process holding cluster RBAC for agents (§3).
  - `CLAUDE.md`'s forbidden "Fleet-managed `infra-bootstrap` cluster ops" (`CLAUDE.md:231`, `docs/DECISIONS.md:467-470`),
    together with `infra-bootstrap`'s own `CLAUDE.md` rule that a human runs Ansible, as ADR-0072 §6 already requires.
    The change is scoped to the catalogue's named playbooks run through ADR-0072.
- **Related:** [ADR-0062](0062-defined-agents.md), [ADR-0071](0071-events-reach-sessions-through-the-bridge.md),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md)

## Context

Agents are stochastic: the same input does not give the same output. That lets them diagnose and repair what nobody
scripted, and it is also why nothing an agent says about its own intent can be the security boundary. Prompt injection
is unsolved: a model cannot reliably tell an instruction from data, and adaptive attacks have broken the published
defenses that rely on the model or on a classifier in front of it.

The field's practical answer is to design the capability graph rather than trust the model. See:
- Meta, *Agents Rule of Two* (ai.meta.com/blog/practical-ai-agent-security);
- Willison, *The lethal trifecta* (simonwillison.net/2025/Jun/16/the-lethal-trifecta);
- Sietsma, *Agentic AI and security* (martinfowler.com/articles/agentic-ai-security.html);
- Beurer-Kellner et al., *Design patterns for securing LLM agents* (arxiv.org/abs/2506.08837), the action-selector
  pattern in particular.

The fleet has a deterministic shell already:
- core authorizes every CoreService call per method and per lease (ADR-0057);
- pods hold placeholders and the vault swaps the real tokens (ADR-0065, ADR-0075);
- core keeps the transcript, so it knows who wrote each session's trigger.

What core does not see is a session's reads and most of its writes. A repo pod's Bash reaches GitHub through the
vault and any other host directly (ADR-0075 §7). `gh`, `curl`, `WebFetch` and the browser stay available
(ADR-0082 §3). A rule that claimed to follow what a session reads would be false. The owner already rejected that
kind of rule: "not truly securing anything but will be really annoying" (ADR-0082 §3, 2026-11-02).

**Guests never reach the fleet.** The owner: "Guest will have pre conceived direct LLM call, 0 access to repos or
anything!" Game guests talk through the game's own voice path. The fleet's untrusted inputs are therefore:
- alerts and similar machine events;
- T2/T3 subjects (ADR-0082 §2).

## Decision

### 1. The rule, and what it governs

In one session an agent holds at most two of:
- **A**, untrusted input as the session's trigger: an alert or similar event, a T2/T3 subject;
- **B**, sensitive reads that core or an executor serves: cluster reads, `ViewLogs`, `QueryMetrics`, private memory;
- **C**, effects that core or an executor runs: catalogue actions, executor steps, `OpenIssue`, `SpawnWorker`,
  `Speak`, memory writes.

A session that would need all three runs only **supervised**: an owner-chosen card option (§4).

**Scope, stated plainly.** The rule governs a session's trigger and the methods core and its executors serve. It does
not govern what a session reads or writes through its own tools: Bash, `gh`, `git push` through the vault, open egress.
- That is ADR-0082 §3, unchanged.
- It is bounded instead by the definition's ceiling (ADR-0062), the vault rules (§7) and the egress lock.
- **Accepted risk:** untrusted text a session reads through a tool sits in its context next to B and C.
- **Revisit trigger:** the first incident, or the first definition that both reads public content and holds a
  vault write to a repo other than its own.

### 2. Core decides the set at create, stores it, and it only narrows

- **Tiers**, matching ADR-0082 §2:
  - **T0, the owner:** the `actors.allow` GitHub login (label or @mention, ADR-0082 §1), console and admin-origin
    surfaces (`FLEET_ADMIN_ORIGINS`, `core/internal/config/config.go:341`), islands for platform-admins, an
    owner-created schedule, and the owner's choice of a card option.
  - **T1:** a fleet `PublishEvent` whose cause was T0 or T1 and whose publisher's set lacks A (amends ADR-0082 §3's
    Publish rule). Also an `Invoke` from a client or from a fleet session whose set lacks A.
  - **Untrusted:**
    - an Alertmanager alert or similar machine event through the bridge;
    - a T2/T3 subject;
    - a `PublishEvent` or `Invoke` from a session whose set holds A.
- **The set.** Core computes it once, when it creates the session, from the trigger's tier and the definition's
  ceiling, and stores it on the row (migration: `sessions.trust_set`).
  - A warm reads it and never recomputes it, so a re-warm cannot widen it.
  - **T0 or T1:** B and C, within the ceiling.
  - **Untrusted:** A plus the one letter the definition's `trust.untrusted` names (`B` or `C`).
  - A definition with no `trust.untrusted` cannot be triggered by untrusted input: core files a card instead.
  - **Repo sessions** (no definition, ADR-0075) take the fleet default ceiling: B and C for T0, and they are never
    triggered by untrusted input, as today (ADR-0082 §1).
- **Context entries do not narrow.** A T2/T3 comment arriving as context of T0 work (ADR-0082 §2) leaves the set as it
  is. ADR-0082 §3 stands, and context entries are covered by §1's accepted risk.
- **One-way.** Nothing widens a stored set. Only a new session gets a new set.
- **Enforced in core and the executors.** Each CoreService and executor method is tagged B, C or neither. Core
  refuses a method outside the set with `PermissionDenied`, key `agent.rule_of_two`, whose guidance names the card
  path.
  - A sidecar-local tool that reaches B or C asks core first. That covers `kubectl_read` and the memory tool, and it
    fails closed.
  - Exposure in the sidecar's tool list is a convenience, not the check.
- **Visible.** The console and the game cockpit show each session's set (`A·B`, `B·C`, `supervised`), and every effect
  in the feed carries it.

### 3. First responders and the catalogue

- **The Alertmanager route.** A dedicated route sends to the bridge with `group_wait 30s`, `group_interval 5m` and
  `repeat_interval 4h`.
- **A first responder is a single-shot run** (ADR-0070) of a definition with `trust.untrusted: B`.
  - The dispatcher, an in-process principal, `Invoke`s one per alert group with the key
    `(dispatcher, alert:<groupKey>)` (amends ADR-0070 §1). A repeat inside the run's life is the same run.
  - `<groupKey>` is a stable hash over `alertname`, `namespace` and `service`.
  - A cooldown of 10 min per key holds further firings and coalesces them, without dropping them. A `resolved`
    alert passes once the alert has been firing for at least `group_interval`.
  - This amends ADR-0082 §1 for those definitions only. Every other definition still never wakes on an event type.
  - Its set is **A·B**: it reads logs, metrics and cluster state.
  - It holds no C method, and its vault carries no GitHub write (§7).
- **Its output is a typed union** (ADR-0070 `output.schema`):
  - a **catalogue selection**: an action plus its parameters;
  - a **card request**: a kind from a fixed enum, plus typed findings (enums, numbers, object ids);
  - **nothing to do**.
  Core turns a selection into a `remediations` row and a card request into an ADR-0082 card it files itself. The
  responder never drafts a card.
- **Checks are split.**
  - **Core checks** the schema, the rate limits and the denylist, then writes a single-use `remediations` row.
  - The **remediation executor** pulls the row by id under its own ServiceAccount (ADR-0072 §2's shape). At pull time
    it checks that the object exists, the node count, and that the target's Application is not under `selfHeal` where
    that matters. It then runs the action once and records the inverse.
  - Core holds no cluster RBAC (`CLAUDE.md:56`).
- **The executors' roles in `infra-bootstrap`:**
  - The remediation executor takes the mutate verbs `thot-executor` holds today, plus what the catalogue needs:
    - `apps/deployments`, `statefulsets`, `daemonsets`: `get`, `patch` (restart annotation, `spec.replicas`, the
      low-stakes field allowlist below);
    - `apps/deployments/scale`: `get`, `update`;
    - `pods`: `get`, `delete`;
    - `argoproj.io/applications`: `get`, `patch` (the `operation` field, sync only).
    - Nothing else: no `nodes` writes, no `secrets`, no RBAC objects, no `*/exec`, no `*/proxy`.
  - `thot-executor` keeps only its read rules. RBAC becomes its boundary.
- **A live gap today, closed by the first executor slice.** The kubectl shim sends `ReadOnly: false` for every call
  (`executor/cmd/kubectlshim/main.go:54-59`). The executor runs that path as a dumb pipe
  (`executor/cmd/executor/main.go:44-53`), on the premise that a human approved the argv through `canUseTool`. In
  `auto` (ADR-0053, ADR-0061) nobody does, so a cluster-access session can patch or delete unattended. Moving the
  mutate verbs to the remediation executor closes this.
- **The actions, each with its real check and inverse:**

  | Action | Dry-run | Inverse | Note |
  |---|---|---|---|
  | restart a Deployment or StatefulSet | server-side dry-run of the patch | none needed | reversible "yes": a rolling restart is its own undo |
  | scale within the definition's bounds | server-side dry-run | scale back to the recorded count | refused on a `selfHeal` Application, which would revert it |
  | delete a crash-looping pod | `get` shows owner and phase | its controller recreates it | only pods owned by a ReplicaSet or StatefulSet |
  | re-sync an ArgoCD Application | `get` shows the diff state | none | sync to git's current revision, **without** prune; reversible "partial" |

- **No cordon.** Node writes stay `node-drainer`'s alone (`infra-bootstrap` ADR-0032, agent-fleet ADR-0035).
  **Owner to confirm**: cordon could return only by amending `infra-bootstrap` ADR-0032 there first.
- **No rollback action.** Under `selfHeal: true` (16 Applications in `infra-bootstrap/gitops`) ArgoCD refuses a
  rollback, and a manual change drifts back. A rollback is therefore a revert PR, filed as a card.
- **Bounds, checked by core:**
  - one action per target per 10 min;
  - 6 actions per responder definition per hour;
  - a target denylist from `infra-bootstrap`: core, the provisioner, Agent Vault, authentik, the databases, the
    ArgoCD controller and the remediation executor itself.
- **Low-stakes namespaces.** A reviewed list in `infra-bootstrap` (`gitops/bootstrap/fleet-low-stakes.yaml`) names the
  namespaces where a catalogue selection may also be a free-form fix.
  - Allowed: a patch limited to a field allowlist (`spec.replicas`, container `resources`, non-secret `env` values, the
    restart annotation), `delete pod`, and a restart.
  - Each such namespace carries the PodSecurity `restricted` label.
  - A namespace is low-stakes only by an owner commit to that file. The game and dev apps start there.
- **Anything else** becomes a card (§4): a code fix, an Ansible step, an unknown failure.
- **The catalogue grows through the owner.** When a card fix keeps being chosen, the owner promotes it into the
  catalogue as a reviewed change. Agents never write catalogue files (§5).

### 4. Cards: what the owner approves

The owner's "proposal" is an ADR-0082 card: a durable entry in the decision deck with self-contained context. One
interface and one deck stay.
- **Context is typed and written by core.** The card's `verified` and `pinned` fields carry the alert name and
  labels, numbers, and object ids that core resolved (ADR-0082 §5).
  - Findings are typed fields: enums, numbers and ids that core checks.
  - Any free text is labelled "agent claim" (ADR-0082 §5) and capped at 280 characters.
  - Raw log lines and alert text never go in.
- **New effect verbs** (ADR-0082 §6, `core/internal/decisions/card.go:188-197`):
  - **`catalogue`:** an action plus its parameters, hashed. Core writes the `remediations` row and the executor runs
    it.
  - **`executor_step`:** an ADR-0072 approvals row binding argv, the `infra-bootstrap` SHA, the directory and the
    environment.
    - The executor pulls and clones that SHA.
    - SSH steps get a per-step authentik JWT in the `netbird-agent` pattern (`infra-bootstrap` ADR-0055).
    - No SSH key is held anywhere.
    - Only playbooks the catalogue names. ADR-0072 §6's first one is `build-runner-configure.yml`.
  - **`open_pr`:** a branch pushed by a B·C session, shown with its diff. A session holding A never files one.
  - **`open_session`:** an optional follow-up session, seeded with the card's typed context posted as an `agent`-authored
    context entry, never as a `human` one. It holds only the letters the owner ticks on the confirm page, so B·C at most.
    It is marked **supervised** in the console and the cockpit, and the owner steers or stops it from the console.
- **The alert webhook.** `/webhook/alertmanager` keeps filing alert proposals.
  - Today `OpenFromProposal` posts the proposal's body as a `human` entry (`core/internal/dashboard/server.go:419`).
    For an alert proposal, the body is posted as an `agent`-authored context entry labelled with its source instead,
    and the opened session is marked supervised: the owner's click with alert text inside.
  - Once responders ship, the webhook skips any alert that a responder definition subscribes to, so nothing is filed
    twice.
- **Choosing runs exactly the bound effect.** Core re-checks the target when the owner answers and again before
  running; a moved target makes the card stale (ADR-0082 §6).
- **The game** stays an ADR-0082 §7 surface for cards: list, watch and answer, with only reversible "yes" answered in
  place. Steer and stop stay in the console, behind a link.
- **Load.**
  - ADR-0082 §8's daily card budget is split per trigger tier, so cards filed for untrusted triggers cannot spend the
    owner's own.
  - A card subject for an alert is `alert/<groupKey>`, a new `CardSubject` kind (proto).
  - ADR-0071's event proposals keep their meaning, an event no session took, and `proposals.source` needs no new
    value.

### 5. Memory and skills are code

- **Each long-lived agent's memory** is a git repo that the sidecar writes, one commit per write, tagged with the
  session and its set.
  - It lives at `claude-home/agent-<name>-memory` on the shared volume.
  - It has no remote and no credential.
  - It is mounted read-only, and only into a session whose set holds B.
- **A session whose set holds A writes only to the quarantine directory**, `claude-home/agent-<name>-quarantine`.
  - That write is tagged neither B nor C.
  - The directory is never mounted into a session whose set holds B or C.
  - The owner promotes from it by reviewing a diff in the console, once a day or on demand. Promote and revert run
    through the sidecar tool, the single writer.
- **`knowledge_journal`** (ADR-0055) is fleet-wide shared memory too. A session whose set holds A writes its notes
  with `quarantined = true` (migration). `journal_search` hides quarantined rows until the owner promotes them.
- **Skills, prompts and pack content** from a session whose set holds A arrive only as a PR.
- **Protected paths.** `CODEOWNERS` plus a ruleset require the owner's review on:
  - `definitions/`, `packs/`, `fleet-shared/`, `.claude/`, `.github/CODEOWNERS`, the ruleset itself and the catalogue;
  - in `infra-bootstrap`, the vault, egress, low-stakes and RBAC files.
  - The `ukubi-fleet` App sits in no bypass list.
  - These paths are excluded from ADR-0082 §9's merge right.
  - No agent changes its own set, tier, catalogue, vault or egress.

### 6. Credentials: no secret in any pod's env

- Today, anyone who can `get pods` can read the per-warm credentials:
  - `LEASE_ID` (`provisioner/internal/k8s/pod.go:485`, and the sidecar's `pod.go:619`);
  - the definition worker's `HTTPS_PROXY` (`vaultWorkerEnv`, `pod.go:384-386`);
  - the repo sidecar's `FLEET_VAULT_PROXY` (`pod.go:845`);
  - a cluster-access worker's `THOT_AUTH_TOKEN` (`provisioner/internal/k8s/ingredients.go:77`).
- **The fix:**
  - The provisioner writes them into a per-pod Secret before the Job, and adopts it with an owner reference after.
  - The Secret is mounted as a file with no subPath. Env holds paths only.
  - The worker's proxy goes through the sidecar's egress router (`127.0.0.1:9093`) for definition pods too, so only
    the sidecar reads the token file, per connection.
  - Automount of the namespace service account token goes off for every pod.
  - The golden tests check mounts as well as env.
- **This is the first PR,** before any new read role. It also closes the exposure to today's `thot-executor` role,
  which can `get pods` cluster-wide.

### 7. Vault

- **First responders' definitions** have a vault with no GitHub write. Their outputs reach the owner only through
  core (§3, §4).
- **Every other definition** keeps one vault for every session class, GitHub write included.
  - **Accepted risk:** a session triggered untrusted with `trust.untrusted: C` can write to GitHub as its
    definition, and a repo pod can send data to any host.
  - **Revisit trigger:** §1's trigger, or the first public-facing input class.

### 8. What is never the boundary

- A classifier, the model's own resistance, or a prompt: they are layers.
- A quiet record.
- An agent's judgement of its own permissions.

## Alternatives considered

- **Per-turn cause windows** (the ADR-0084 drafts). They went through five doubt rounds of blockers. Rejected: the
  decision belongs at the start, where core decides alone.
- **Read-taint or a one-way switch on reads.** Core cannot see a pod's own reads, and the owner rejected taint
  (ADR-0082 §3).
- **Pods per trust tier.** The pods shared a vault and a definition, and doubled the memory. Rejected.
- **Free-form remediation from alerts.** It is A+B+C. Kept only for the owner's low-stakes namespaces, with a field
  allowlist and the same executor.
- **Cordon in the catalogue.** It would breach `infra-bootstrap` ADR-0032's node rule. Left out.
- **CaMeL-style value tracking:** the principled end state, research-grade for one owner.
- **Per-action approval:** people approve nearly everything they are asked. Cards are kept for what the catalogue
  does not cover.

## Consequences

- Trust is one stored field per session, visible in the console and the cockpit.
- Self-healing runs unattended inside the catalogue's bounds. Anything else is a card whose option runs exactly what
  it shows.
- `thot-executor` becomes read-only, which closes today's unattended-mutation gap.
- New pieces:
  - core: method tags, `sessions.trust_set`, the `remediations` table, three card verbs, the `alert` card subject;
  - the remediation executor and its ClusterRole in `infra-bootstrap`;
  - memory quarantine and the journal flag;
  - the per-pod Secret file.
- The accepted risks (§1, §7) stay written here until a trigger fires.

## The bet

That deciding trust once, from what core knows for certain (who wrote the trigger), and letting a stochastic agent
only choose from a bounded menu, gives real self-healing without trusting the agent. And that the catalogue covers
most alerts, so the owner's attention goes to the cards that matter.

## Not doing

- Read-taint.
- A classifier gate as a boundary.
- Agent-editable policy.
- Cordon, or a rollback against ArgoCD self-heal.
- Guests in the fleet.
- Vault rule sets per session class beyond responders.
- Standing SSH keys.
