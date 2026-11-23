# ADR-0086 — Rule of Two: core sets each session's trust from its trigger

- **Status:** Accepted (owner, 2026-11-16)
- **Date:** 2026-11-16
- **Amended 2026-11-21 (owner):** verb kinds and `values.set`'s CI render check (§3, §4), AGit proposals (§4),
  probation for §5's protected paths, the Forgejo bot users
  ([ADR-0075](0075-github-app-identity-and-vault-credentials.md)) and the CI/CD split as a precondition; see the
  amendment, a decision record whose mechanisms are open items.
- **Amendment 2026-11-23 (Proposed, waiting for the owner):** write cards answered in the game through core's card
  frame, a per-verb deadline set at filing, refusal and expiry at it, never an approval on expiry, and the answering
  surface recorded; amends §4 and ADR-0082 §5, §7 (see the amendment).
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
  - *Added 2026-11-21 (the amendment):*
    - ADR-0082 §9: on Forgejo, `ukubi-review`'s approval is the only agent approval that counts, and agents merge
      after a fleet review on §5's paths too, under probation once its controls are live (D3, D7);
    - ADR-0082 §11: on Forgejo, the forge write path holds the bot users' tokens, not the App key, and the
      `ukubi-review` token stays with the provisioner (D3);
    - [ADR-0075](0075-github-app-identity-and-vault-credentials.md): on Forgejo, bot users `ukubi-fleet` and
      `ukubi-review` replace the App (D3);
    - ADR-0065 §7 and its rollout: on Forgejo, `git.mode: push` proposes through AGit, since `ukubi-fleet` pushes no branch (D3).
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
    `(dispatcher, alert:<groupKey>)` (amends ADR-0070 §1). A retry of a full fleet is the same run.
    **Amended in S06:** the key is `alert:<groupKey>:<definition>:<window>`, the window being the cooldown window the
    run starts. Two responders on one group are two runs, and firings held through the cooldown start the next run
    while the first may still be live, so runs for one group can overlap. The dispatcher invokes as the bridge client,
    so responder runs count against the bridge's parked and per-client caps, shared with the PR reviewer.
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
    - `apps/deployments`, `statefulsets`, `daemonsets`: `get`, `patch` (restart annotation, `spec.replicas`; the RBAC verb, not a catalogue action *(Amended 2026-11-22: the field allowlist is gone)*);
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
  namespaces where a catalogue selection may also be a free-form fix. *(Amended 2026-11-22: with patch removed
  there is no free-form fix, so the list gates nothing. Core no longer reads it; `delete pod`, restart, scale and
  sync apply in any namespace off the denylist. The file stays in `infra-bootstrap`, inert, until a free-form action
  returns.)*
  - Each such namespace carries the PodSecurity `restricted` label.
  - A namespace is low-stakes only by an owner commit to that file. The game and dev apps start there.
    *(Amended 2026-11-21, owner: once probation's controls are live, an agent's change to this file merges on
    probation; see D7.)*
- **Anything else** becomes a card (§4): a code fix, an Ansible step, an unknown failure.
- **The catalogue grows through the owner.** When a card fix keeps being chosen, the owner promotes it into the
  catalogue as a reviewed change. Agents never write catalogue files (§5).
  *(Amended 2026-11-21, owner: once probation's controls are live, an agent's change to them merges on probation;
  see D7.)*

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
    *(Amended 2026-11-21: on Forgejo, the B·C session's AGit PR in place of a branch; see D3.)*
  - **`open_session`:** an optional follow-up session, seeded with the card's typed context posted as an `agent`-authored
    context entry, never as a `human` one. It holds only the letters the owner ticks on the confirm page, so B·C at most.
    It is marked **supervised** in the console and the cockpit, and the owner steers or stops it from the console.
- **Built in S07:** the card verbs are filed by core alone, so the sidecar's `draft_card` still offers `handoff` and
  `close_threads` only.
  - Each carries a canonical `payload` and its `payload_hash` (sha256 of verb and payload). Core checks the hash
    again when the owner answers; a payload that moved is stale and the card stays open.
  - A surface never answers one in place, even if reversible "yes": it goes through the confirm page.
    *(Proposed amendment 2026-11-23: a surface answers one only through core's card frame or the confirm page; see E1.)*
  - `catalogue` runs `remediations.Service.Select` for the writer's definition (the schema, denylist and rate limits
    again, the executor's checks at pull). `open_pr` is the provisioner's `OpenPullRequest`: it refuses a branch that
    moved past the approved commit, and core files and runs it only for a `BC` writer.
    *(Amended 2026-11-21: on Forgejo no branch exists to open from. How `open_pr` binds an AGit PR, and how a
    pr-mode session's commit reaches the forge, are open items of the amendment.)*
  - `open_session` starts a session at T0 trust (`BC`) with the typed brief as an `agent` entry, the title labelled an
    agent claim. The letter ticking on the confirm page is not built: it gets `BC`. It launches in `default` permission
    mode (every tool asks, never the unattended `auto`), with `[supervised]` on its title. A console session is
    admin-only, so every admin is its owner; `sessions.owner` is a definition-thread field (a DB check), so no per-owner
    field is set until a migration adds one.
  - Before the answer is recorded, core checks what can fail: `Check` (the catalogue's refusals and rate limit), the
    target repo startable, and for `open_pr` that the PR's repo is the writer's own. An effect that still fails
    after the answer (a busy fleet, a provisioner outage) is written to the card's thread as a note; the card is closed,
    so core files a new one.
  - The confirm page and the console show the canonical payload's fields beside the hash, the title labelled an agent
    claim, and for `open_pr` the diff link for the approved commit. Branch names follow git's ref-name rules in core and
    in the provisioner.
  - `executor_step` is declared and refused, in the error "until ADR-0072's executor exists".
  - The `alert` subject is the `alert_group` kind S06 added (thread key `alert/<groupKey>`).
- **The alert webhook.** *Amended 2026-11-23 by [ADR-0087](0087-alerts-reach-the-fleet-only-through-responders.md): core's webhook and every alert proposal are gone; an alert reaches the fleet only through a responder.* `/webhook/alertmanager` keeps filing alert proposals.
  - Today `OpenFromProposal` posts the proposal's body as a `human` entry (`core/internal/dashboard/server.go:419`).
    For an alert proposal, the body is posted as an `agent`-authored context entry labelled with its source instead,
    and the opened session is marked supervised: the owner's click with alert text inside.
  - Once responders ship, the webhook skips any alert that a responder definition subscribes to, so nothing is filed
    twice.
- **Choosing runs exactly the bound effect.** Core re-checks the target when the owner answers and again before
  running; a moved target makes the card stale (ADR-0082 §6).
- **The game** stays an ADR-0082 §7 surface for cards: list, watch and answer, with only reversible "yes" answered in
  place. Steer stays in the console, behind a link.
  *(Proposed amendment 2026-11-23: card verbs are answered in the game through core's card frame, before their
  deadline; see E1 to E5.)*
  *(Amended 2026-11-17, owner: start and stop for characters move to `AgentService`, owner-only; see [ADR-0084](0084-long-lived-agents.md) amendment 2026-11-17.)*
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
  *(Amended 2026-11-21: a session holding A proposes no lasting change (D6), so they arrive as a card request or in
  the quarantine directory above.)*
- **Protected paths.** `CODEOWNERS` plus a ruleset require the owner's review on:
  - `definitions/`, `packs/`, `fleet-shared/`, `.claude/`, `.github/CODEOWNERS`, the ruleset itself and the catalogue;
  - in `infra-bootstrap`, the vault, egress, low-stakes and RBAC files.
  - *(Amended 2026-11-21: the provisioner's and core's forge, probation and enforcement code join the list; see the
    amendment's invariants.)*
  - The `ukubi-fleet` App sits in no bypass list.
    *(Amended 2026-11-21: on Forgejo, `ukubi-fleet` is a read collaborator in no push or approval allowlist; how
    the owner-only paths are gated there is P2.2 and P5.)*
  - These paths are excluded from ADR-0082 §9's merge right.
  - No agent changes its own set, tier, catalogue, vault or egress.
  - *(Amended 2026-11-21, owner: once probation's controls are live, the owner's review above becomes probation
    (D7), except on the amendment's owner-only paths, which include the vault, egress and RBAC files. An agent's
    change to its catalogue then merges on probation. Set and tier are fed by a definition's `trust`, a
    client's `actors.allow` (`definitions/clients/*.yaml`) and the reach fields (`invokers`, a client's
    `definitions`, `subscribes`); an agent change to those fields is owner-only (see the invariants).)*

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
    the sidecar reads the token file, once at start (the token is per warm and never rotated).
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
  - *(Amended 2026-11-21: a session holding A never proposes a lasting change (D6). How that is enforced against
    per-definition vaults is an open item, so this risk stands until it is built.)*

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
  allowlist and the same executor. *(Amended 2026-11-22: not built; patch was removed and with it the allowlist
  and the low-stakes gate.)*
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
- Agent-editable policy. *(Amended 2026-11-21, owner: on probation for §5's paths once its controls are live, never
  for the owner-only paths; see D7.)*
- Cordon, or a rollback against ArgoCD self-heal.
- Guests in the fleet.
- Vault rule sets per session class beyond responders.
- Standing SSH keys.

## Amendment 2026-11-21: verb kinds, AGit proposals, probation, the Forgejo identity

Owner decisions of 2026-11-21 (ukubi conception §16, build plan "Owner decisions (2026-11-21 late)"), taken while the
forge moves to Forgejo (`git.bnei.dev`). This records decisions and invariants only. None of it is built: each
mechanism is designed with its code in the build task named under "Open items". The text above stands wherever this
does not change it.

*Patch removed 2026-11-22: the executor never implemented it; a settings change is a lasting `values.set` PR (D2) or
a later executor case. Core no longer accepts the action (migration 000039), and the catalogue is restart, scale,
delete_pod and sync_app.
Migration 000039 adds its CHECK `NOT VALID`, so finished patch rows stay as history. The migration that removes
`scale` must also be `NOT VALID`, or first delete those rows.*

### Decisions

- **D1. Verbs are lasting or transient.** A lasting verb changes desired state through git, and its PR is the
  reversible act (its undo is a revert PR); a transient verb acts on the running system and is its own undo or records
  its inverse. The catalogue's four actions are transient; `sync_app`'s undo is partial (§3).
- **D2. `values.set` gets a render check that runs in CI:** a required PR check job, with no secrets and on no
  privileged runner. It never runs in the provisioner, which holds forge tokens.
- **D3. Agents propose through AGit** (`git push origin HEAD:refs/for/<base>/<topic>`), never from a fork or a
  branch. `ukubi-fleet` is a read collaborator holding a `write:repository` token: the token allows the AGit push, and the read role makes
  Forgejo's pre-receive refuse any branch or tag push (verified). `ukubi-review` is a separate user whose approval is the only agent approval that counts; no component
  but the provisioner holds its token at runtime (a mounted file).
- **D4. Verified on Forgejo 15.0.12** (build plan, AGit spike): Forgejo Actions runs an AGit PR as a fork PR
  (`is_fork_pull_request: true`); its run gets no repo secrets but a job token; every run waits for approval unless
  the poster is trusted, and approval is web-only; "Approve always" is ignored for a restricted user and stored per
  repo; an approved run can pick any runner label.
- **D5. The CI/CD split is a precondition** of trusting `ukubi-fleet` and of resuming the fleet:
  - CI runs checks only, with no secrets, on the podman runner;
  - CD runs only from `main` and tags, through a deployer the `main` push webhook triggers, which holds the
    persistent builder;
  - PR images build on an ephemeral, isolated builder;
  - the host executor is unreachable from PR runs by its runner registration and scope, not by label (D4: the PR
    picks the label).

  Only then does `ukubi-fleet` become unrestricted, with one "Approve always" per fleet repo. The owner never approves
  runs one by one.
- **D6. A session holding A never proposes a lasting change.** It files a card request (§3), and the owner's option
  runs at B·C. The enforcement mechanism is an open item.
- **D7. Agents merge after a fleet review (ADR-0082 §9) everywhere but the owner-only paths and set/tier fields,** §5's paths
  included, which merge under probation once its controls are live. Until then §5 stands. Probation covers
  agent-authored merges only:
  - an immediate notice to the owner;
  - a 24 h window, in which the case's card reverts the merge in one click, or a canary signal (Degraded health, a
    rise in alerts on the System) reverts it automatically. The canary only reverts, at most once per case, and
    only inside the window;
  - an episode card with evidence, until the library exists.
- **D8. The library never opens PRs.** It files lesson candidates as cards.
- **D9. Mistakes come in three tiers.** Cheap: visible and reversible (a bad app PR, a wrong replica count), allowed,
  followed by a revert and a lesson. Compounding: it widens reach (an agent's own definition, packs or catalogue),
  allowed on probation. Irreversible: no undo exists (a leaked key, deleted production data, an exposed secret), never
  an agent's alone: the owner-only paths below, or a decision the owner signs.

### Invariants

Each holds whatever mechanism implements it, and each can be checked.
- An agent merge targets only the repo's default branch: never `deploy`, never a tag.
- No agent merge changes an owner-only path: at least `db/migrations/**`, the secrets, vault, egress and RBAC files,
  `.forgejo/**` and `.github/workflows/**`. Probation never covers them.
- No agent merge changes the fields that feed a session's set and tier: a definition's `trust`, `invokers` and
  `subscribes`, and a client's `actors.allow` and `definitions` (`definitions/clients/*.yaml`).
- A probation revert cannot be closed or edited by any `ukubi-fleet` token holder: it is posted by an identity no
  session holds.
- The provisioner's and core's forge, probation and enforcement code is on §5's protected list.
- The host executor takes no PR job.

### Open items

- A-session proposal enforcement (D6), when vaults are per definition and every warm on one shares its keys (P4.1).
- If D6 is enforced with a separate untrusted vault: `vault.go` makes a read-only vault only from a minted key, for a
  spec naming one forge repo path (P4.1).
- The pr-mode push path: a session's clone is a `git clone --shared` whose alternates point into the repo cache
  (P4.1).
- The render check's source access (D2): `infra-bootstrap` is private and its Applications name it by SSH URL, while
  the PR run holds no credential (P2.4).
- `merge_pr`: the method, who performs the merge for a read collaborator, and the checks it refuses on (P5 step 3).
- Probation case detection: where a merge's changed-file list comes from (P5 step 4).
- `botEvent` keys on a `github.com/` source prefix (`core/internal/dispatch/dispatch.go`), which a Forgejo event
  does not carry (P4.4).
- Dedup between a system webhook and per-repo webhooks, if a system webhook watches for `ukubi-fleet` PRs outside
  the fleet (P4.3).
- AGit topic naming: any `ukubi-fleet` push to a topic updates that topic's PR (P4.2).
- Owner merges during the interim: an admin override or `ukubi-review`'s approval (P2.2).
- The definition ceiling (§1, §2) is not built: `core/internal/sessions/trust.go` gives T0 and T1 B·C whatever
  the definition says, so reach fields decide set and tier today (P5).
- Owner-only gating on Forgejo: the path globs for the secrets, vault, egress, RBAC and enforcement files, the
  field-level check for `definitions/*.yaml` (`trust`, `invokers`, `subscribes`) and
  `definitions/clients/*.yaml` (`actors.allow`, `definitions`), and the merge-time check that refuses them (P2.2, P5).

## Amendment 2026-11-23 (Proposed): write cards answered in the game, and a real countdown

**Status: Proposed, waiting for the owner's approval.** It changes the fleet's trust rules, so nothing here is built
or holds until the owner accepts it. The text above stands until then, and afterwards wherever this does not change it.

Owner decisions of 2026-11-23 (mountain plan/29 §6 and §7, "Owner decisions on §7's pending items", items 2 and 3).
The ukubi game shows an alert as a monster fight, and a write move in the fight pauses on its card with a countdown
(plan/29 §6.2, §6.8). Two rules stand in the way today:
- §4: "A surface never answers one in place, even if reversible 'yes': it goes through the confirm page." ADR-0082
  slice 6: "The game's in-place answering is a later ukubi-game PR."
- ADR-0082 §5: "An answer is a message. Whenever it comes, core appends it." So a countdown in the game cannot mean
  "nothing writes": an answer after it still runs.

The owner's decisions: a write card is answered in the game, not only on the confirm page; when the card's countdown
ends the card is withdrawn, so a late answer cannot run; any admin may answer (`AnswersCards()`, `platform-admins`).
The friends who join a fight are admins, never public guests.

### What the confirm page guarantees today

The confirm page (`core/internal/confirm`, ADR-0082 §7) exists because a script injected into an admin origin can
click whatever it draws. It guarantees four things:
- **G1. Core renders the stored card.** The option, its effect and its fields come from the row, Go-rendered by core,
  never from the surface's own code.
- **G2. Only a human on core's origin submits.** The answer is a signed same-origin POST on core's origin, top level
  only (`frame-ancestors 'none'`). A script on the game origin can neither read the page nor submit it.
- **G3. A recent sign-in.** It needs a sign-in under 10 minutes old (`core/internal/auth/session.go:83`), else it
  sends the admin to log in again (`reauth=1`).
- **G4. Exactly the bound effect.** The answer names the option and the card version, and core checks the payload
  hash again (§4, ADR-0082 §6).

Answering a write card from the game's own script through `AnswerDecision` would keep G4 and lose G1 to G3: an injected
script could answer a write card itself. That is the reason the rule above exists, and it is not given up.

### Decisions

- **E1. The game answers a write card through core's card frame.** A new core path, `/decisions/card?id&v`, renders
  one card with the confirm page's own renderer and answers on the same signed same-origin POST.
  - It is the only response core lets be framed, and only by `FLEET_ADMIN_ORIGINS` (`frame-ancestors` names them
    exactly; every other response keeps `frame-ancestors 'none'`). Islands and `embed.origins` never frame it.
  - The game's owner page embeds it in the dock (a full-width sheet on the phone). The game draws the fight around it;
    the card itself, its buttons and its answer are core's. G1, G2 and G4 hold: the game's script cannot read the
    frame, change what it shows or submit it, since the frame is cross-origin.
  - The frame sends nothing to its parent. The game learns the outcome from `WatchDecisions`, as it learns every card
    change today, and plays the strike then.
  - It applies to the card verbs core files (`catalogue`, `open_pr`, `open_session`) and to any other card option that
    is not reversible "yes". A reversible "yes" option on an agent's card is still answered in place, as ADR-0082 §7
    allows. Answering a card verb through the game's script stays refused with `agent.decision_confirm_required`,
    whose URL now names the frame path as well as the confirm page.
  - The confirm page stays, unchanged, for the console's links and for any admin who wants it top level.
- **E2. What the frame shows before a write answer.** All of it comes from the stored card:
  - the verb and its typed fields: the catalogue action and its parameters, the target's object ids, for `open_pr`
    the repo, the approved commit and the diff link, for `open_session` the letters it starts with (`BC`) and that it
    is supervised;
  - each option's reversibility (`yes`, `partial`, `no`) and the effect in words from core's fixed template ("restart
    Deployment `x` in namespace `y`");
  - the writer (the responder definition and its set, `A·B`), the agent's text labelled "agent claim", and the short
    payload hash;
  - the countdown, counted down from the card's `expires_at` as core sent it (E4), never from a timer the game sets;
  - an irreversible option takes a second click, as in the console.
- **E3. A recent sign-in, sized for a fight.** The frame answers only under a sign-in under 30 minutes old (default,
  owner may change), checked from the session's `auth_time` as the confirm page does. When the fight starts, the game
  sends the admin through a top-level login with `reauth=1` if the sign-in is older, so no countdown is spent on a
  login. The confirm page keeps its 10 minutes. A frame past the window shows "sign in again" and answers nothing.
- **E4. Every card verb core files has a deadline, set when it is filed.** Core writes `expires_at` from the verb,
  not from `CardTTL` (`core/internal/decisions/card.go:65`, 7 days). Defaults, owner may change:

  | Verb | Deadline | Why |
  |---|---|---|
  | `catalogue` (transient, reversible `yes` or `partial`) | 10 min | it acts on the system as it is now; the responder's cooldown is 10 min (§3), so the next run on a group that still fires files a fresh card with fresh findings |
  | `open_pr` (lasting, undone by a revert PR) | 24 h | a change through git waits for the owner; core still refuses a branch or PR that moved |
  | `open_session` | 24 h | it starts a supervised session; nothing in the world moves meanwhile |
  | `executor_step` (refused today) | 10 min | when built, the same as `catalogue`, since a playbook step acts on the system now |

  - A card holding an irreversible option never gets a longer deadline than the table gives its verb.
  - Agent-drafted cards (`handoff`, `close_threads`) and core's other cards (budget, unmerged close) keep `CardTTL`.
    This amendment does not shorten them.
  - Nothing extends a deadline. A card that expires is gone; a new card is a new decision.
- **E5. At the deadline the card is withdrawn, and a late answer cannot run.**
  - The answer is recorded only if it lands before `expires_at`, by the database's clock, in the same statement that
    records it: the update in `decisions.go` (the `state = 'open'` update) also requires `expires_at > now()`, and core
    runs the effect only when that update changed the row. Today the check is on the record read before the answer,
    in Go time (`answer.go:109`), so an answer racing the deadline could still be recorded; that gap closes.
  - Past it, every answer path (the frame, the confirm page, the console, `AnswerDecision`) is refused with
    `agent.decision_expired` and does nothing. The frame shows "This card expired at <time>. Nothing ran."
  - `ExpireCards` (`card.go:119`) turns it `expired`, which `WatchDecisions` sends at once. `expired` is ADR-0082 §7's
    "closes as no action", and it is what the owner calls withdrawn; the `withdrawn` state keeps its meaning (the
    session archived, or the agent withdrew). It runs today on core's 60 s session tick (`core/cmd/core/run.go:200`);
    it moves to its own 15 s tick (default), so a game showing 0 s sees the card leave within that. The refusal does
    not wait on the sweep.
  - An answer recorded before the deadline runs its effect even if the effect finishes after it. The decision is the
    answer's time, not the effect's.
- **E6. Expiry never approves.** No option runs on expiry, the recommended one included. There is no default answer,
  no "approve if nobody objects" and no auto-approve on any surface. What happens on expiry is what happens if the
  owner says nothing: nothing writes (ADR-0082 §7, and plan/29 §6.2: "the mob gains ground on screen only, never in
  production").
- **E7. What the agent sees.** The responder that asked for the card is a single-shot run that has ended (§3), so no
  session waits on it.
  - On expiry core appends a note to the card's thread from a fixed template: "card c expired at <time>, no action".
    This is the same path as an effect that fails after the answer (§4).
  - `list_responder_cards` and `ListDecisions` show the card as `expired`, with its `expires_at`. A character (ops-lead
    in a fight) reads that and proposes again only through a new responder run or its own card path; it never re-files
    a card verb (core files them alone).
- **E8. `DECISION_WAIT` is not the countdown.** It stays what ADR-0082 §5 says: how long an asking session waits before
  it carries on. It applies to a session's own question, permission and plan decisions, and their answers still arrive
  as messages whenever they come. A card verb has no asking session and no `DECISION_WAIT`; its countdown is `expires_at`
  (E4). ADR-0082 §5's "An answer is a message. Whenever it comes" now excludes cards with a deadline, which are refused
  after it.
- **E9. Who answered, and from where, is recorded.** Each answer stores `answered_by` (the admin's login, as today),
  plus, in a migration:
  - `answered_via`: `console`, `confirm` or `frame:<admin origin>`;
  - `auth_time`: the sign-in's time, which E3 checked.
  The console's card history, the card's thread note ("owner chose option k of card c", ADR-0082 §6) and the fight's
  transcript show the login and the surface. An expired card records no answerer: `answered_by` stays empty and the
  state is `expired`.
- **E10. Any admin answers.** The frame and the confirm page accept `AnswersCards()`: a `platform-admins` member in the
  console or on an admin origin. The game server's client bearer, an island and a guest are refused, as today. The
  friends in a fight are admins, so they answer like the owner; each answer carries its own login (E9).

### What stays

- **Guests never reach the fleet.** A public guest never sees the frame, a card or a fight's transcript (plan/29 §7
  decision 3).
- **Card verbs are filed by core alone.** The sidecar's `draft_card` still offers `handoff` and `close_threads` only.
  The responder still asks through its typed output (§3), and the alert-responder remains the scout: nothing here
  changes who files cards or what a responder may hold (`A·B`, no C method).
- **The Rule of Two holds.** An answer is a T0 choice of one bound effect (§1 "supervised"). `open_session` still
  starts at `BC`, supervised, in `default` permission mode. No session's set widens.
- **Steer stays in the console.** Stop, start and close of characters are as ADR-0084 A4 has them. The fight's
  "redirect" is a message, not a card answer, and its path is not decided here.
- **Re-checks stay.** Core checks the payload hash and the target when the answer is recorded and again before running
  (§4); a moved target makes the card stale.
- **No new surface call.** The frame is a core-served page, not an `AgentService` method; `surfaceCalls` is unchanged.

### Would amend

- §4 of this ADR: "A surface never answers one in place … it goes through the confirm page" reads: a surface answers one
  only through core's card frame (E1) or the confirm page. "The game stays an ADR-0082 §7 surface … with only reversible
  'yes' answered in place" gains: and card verbs through the frame.
- [ADR-0082](0082-decision-cards.md):
  - §5, "An answer is a message. Whenever it comes": not for a card with a deadline (E5, E8);
  - §5's table: `expires_at` is set per verb for core's card verbs (E4); the answer fields gain `answered_via` and
    `auth_time` (E9);
  - §7: core's card frame is the one response an admin origin may frame (E1), with its own sign-in window (E3);
  - slice 6's "The game's in-place answering is a later ukubi-game PR": it is the frame, embedded by that PR.
- `CLAUDE.md`'s auth row and ADR-0082's `frame-ancestors 'none'` everywhere: one path is framable by the admin origins.

### Consequences

- The game can show a write card in the fight and answer it there, with the same four guarantees as the confirm page,
  G3's window widened from 10 to 30 minutes.
- The countdown is real: what the game shows is `expires_at`, and core refuses an answer after it.
- An owner who is away misses transient cards. That is intended: they expire, nothing writes, and the next responder
  run files a fresh one while the alert still fires. Lasting cards wait a day.
- One response is framable. A script on an admin origin can still lay out the page so the admin clicks the frame by
  mistake (clickjacking); it cannot click, read or change it. The second click for irreversible options and the typed
  effect text are the checks left.
- The work it implies:
  - **core:** per-verb `expires_at` at filing; the deadline in the answer's update and the effect only on a changed
    row; `ExpireCards` on its own 15 s tick, appending the thread note; the `/decisions/card` frame in `core/internal/confirm`,
    its `frame-ancestors` for `FLEET_ADMIN_ORIGINS`, its 30-minute sign-in check; `agent.decision_confirm_required`
    naming the frame URL; the migration for `answered_via` and `auth_time`; tests for a racing answer, a late answer
    on every path, a frame from a non-admin origin, and the frame's header.
  - **ukubi-game (a later PR):** the frame in the dock, the countdown from `expires_at`, the re-sign-in when a fight
    starts, the strike on `WatchDecisions`' change, and no answer of a card verb from its own script.
  - **console:** `answered_via` in the card history.

### Open questions

- **Owner away versus a short deadline.** plan/29 §6.6 says "with the owner away … every write card waits", and §6.2
  starts the countdown at 60 s. A deadline set when the card is filed cannot be both. E4 picks 10 min for transient
  cards and lets the next responder run re-file. The alternative is a narrowing call: when an admin opens a card in a
  fight, core shortens its `expires_at` to 60 s (never lengthens it). It is safe (shortening runs nothing) but is a
  new surface call. Owner to choose.
- **The sign-in window.** 30 minutes (E3) versus the confirm page's 10. A longer fight needs a re-sign-in mid-fight, or
  a longer window.
- **The frame versus a game-drawn button.** plan/29 §6.8 has "approve (a heavy strike lands the change)". Under E1 the
  click is on core's frame, not on the game's sword. Answering through the game's own script would make the strike
  the answer, but gives up G1 to G3 for every admin origin. A per-answer passkey (WebAuthn, the payload hash as the
  challenge) would keep a human in the loop with a game-drawn button; it needs passkeys in authentik first.
- **Permission and plan decisions.** E8 leaves their late answers running, as the owner's decision names write cards
  only. Under `auto` (ADR-0061) few reach a human; if the owner wants a countdown on them too, it is another amendment
  of ADR-0082 §5.
