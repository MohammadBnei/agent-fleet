# ADR-0084 — Long-lived characters: one pod per character, kept up by core

- **Status:** Proposed (2026-11-16).
  - Rewritten after doubt cycle 2. The owner then dropped multi-thread and two-tier pods for one pod per character,
    and adopted [ADR-0086](0086-rule-of-two.md) (Rule of Two) as the trust rule.
  - Choices marked **default, owner may change** are the coordinator's defaults.
  - **Owner to confirm** marks a consequence the owner has not yet seen.
  - Mechanisms live in the build-slice appendix, each doubted against the code when it is built.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Depends on:** [ADR-0086](0086-rule-of-two.md) (PR #401) for trust, proposals, the catalogue, memory provenance
  and the per-pod Secret file.
- **Would amend:**
  - [ADR-0002](0002-no-orchestration-framework.md): reopened narrowly, by its own revisit clause, for runtimes
    behind the sidecar later (§12).
  - [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) and
    [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1–§2: a character's pod is started once and kept up by
    core (§3).
  - [ADR-0058](0058-an-answer-wakes-the-session.md) and `DECISIONS.md` §2 ("appending to a session that has no live
    pod without warming first"): allowed for character sessions, which keep their own cursor (§3).
  - [ADR-0037](0037-thot-is-a-worker-task.md) and `CLAUDE.md`'s "Fleet's only process holding cluster RBAC" (the
    executor): a character granted cluster reads holds its own read-only kube identity (§6).
  - [ADR-0064](0064-definition-pod-profile.md)'s method table: `ViewLogs` (Loki), forbidden to definitions, is
    allowed to a character that is granted it (§6).
  - [ADR-0062](0062-defined-agents.md) §5/§7: the `standing` lifecycle, its ceiling and its pool (§2, §9).
  - [ADR-0066](0066-agent-service.md) §6: an unknown actor's entry reaches a character as untrusted instead of never
    waking it (§8).
  - [ADR-0045](0045-service-endpoint-roster-direct-dial.md) and `DECISIONS.md` §2 ("a new passthrough RPC on core"):
    `QueryMetrics` is a named exception, because the egress lock blocks a direct dial from a definition pod (§6).
- **Related:** [ADR-0070](0070-headless-invoke.md) (`SpawnWorker` is an in-process `Invoke`),
  [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) (events),
  [ADR-0082](0082-decision-cards.md) (cards inside a session),
  [ADR-0004](0004-subscription-oauth-not-metered-api.md) (one subscription).

## Context

The fleet runs workers: one pod per session, warmed on a message, torn down when idle. The owner wants a second kind,
**characters**: named agents that live on. Examples:
- an ops character that watches alerts, reads logs, metrics and cluster state, opens issues and spawns workers;
- game characters (the world and village residents) that talk with the owner and guests.

Characters need little power: they mostly edit files and pick remediations from ADR-0086's catalogue.

Earlier drafts put several threads, or two trust tiers, inside one pod. Five doubt rounds found blockers each time.
Every fleet invariant (lease, vault, cap, cards) assumes one session per pod, and computing trust per turn from the
transcript leaked. The owner's decision is the simple shape: **one long-lived pod per character, one session per pod,
trust from ADR-0086.**

## Decision

### 1. Two kinds of agent

- **Workers**, unchanged.
- **Characters**: an `AgentDefinition` with `lifecycle: standing`. Each character is one session in one pod, started
  by the owner and kept up by core until stopped. There is no rotation.
- A character's pod is a definition pod:
  - it holds placeholders only;
  - it sits behind the egress lock;
  - it reaches the world only through core and the vault.

### 2. Identity and ceiling

- **A character is a principal of its own, `standing:<name>`.**
  - The definitions loader reserves the `standing:` prefix and refuses it in any client file.
  - The session row is owned by `client:standing:<name>`, which satisfies migration 000015's
    `sessions_invoker_owns` CHECK.
  - The admin who started it is kept in its own column.
- **A standing-only definition is opened only by `Start`**, an admin call from the console. Core refuses `Open`,
  `Invoke` and a routed proposal for it, with `FailedPrecondition`, key `agent.standing_only`.
- **The definition is the ceiling.** Its tools, its B and C methods (ADR-0086 §2) and its `trust.untrusted` choice
  (A plus B, or A plus C) are reviewed config. The agent changes none of them.

### 3. Lifecycle: started once, kept up by core

- **Exempt from the sweeps.**
  - The idle, startup-stall and stop-grace sweeps skip `standing` rows.
  - `limits.timeout` is not an idle timeout for a character.
  - The Job's `activeDeadlineSeconds` stays at the token cap (§10).
- **Re-warm: a new mechanism.** Today's `ListRewarmRuns` covers single-shot `TERMINATED` only.
  - When a character's pod phase is terminal or its warm failed, core re-warms it after `restart_after`.
  - `restart_after` is a timestamp on the row, set on **every** terminal path: `ReportPodEvents`, a failed warm,
    `CRASHED`.
  - A NULL counts as due. Stop clears it and is excluded from the selector.
- **No retry counter (ADR-0048 §2 stands).**
  - The back-off doubles from the pod events of the last hour, read at re-warm time.
  - After the third failure in that hour, core stops re-warming and alerts the owner.
  - Nothing is counted in a column.
- **Its own cursor.**
  - Entries appended while the pod is down are delivered from the character's `consumed_seq`, held by core. It is
    the last result's seq, written by core, never by the pod.
  - They are not delivered from `LatestSeq` at warm. That is why ADR-0058's ordering rule does not bind characters.
- **Replaced, never rotated, on a pin change.**
  - Intake stops, the running turn gets a deadline (default 10 min, **default, owner may change**), then the pod is
    replaced.
  - Revocation and a vault widening tear down at once, as for any definition.
  - The new pod waits for the old pod's lease to end before its sidecar opens the memory repo.

### 4. Turns: one trigger at a time

- **Core delivers one turn-starting entry at a time to a character, and never merges another into a running turn.**
  The next entry waits for the turn's result.
- A turn therefore has exactly one trigger, and ADR-0086 §2 sets its Rule of Two set from that trigger's tier.
- The set can differ turn to turn within one character:
  - an alert turn is A plus the definition's untrusted choice;
  - an owner turn is B·C.
- Core enforces the set at every method call, against the session's current turn, which core knows because the pod
  has one session.
- **Default, owner may change:** owner (T0) entries jump ahead of untrusted ones in the queue. They never interrupt a
  running turn.

### 5. Events

- **Alerts reach characters through the bridge** (finishing ADR-0071's outstanding item).
  - A dedicated Alertmanager route sends to the bridge with `group_wait 30s`, `group_interval 5m` and
    `repeat_interval 4h`.
  - Alerts about the fleet's own characters (runaway turns, crash loops) are excluded from that route and go to the
    owner only.
  - The old `/webhook/alertmanager` path is retired when this lands, so nothing arrives twice.
- **Subscriptions wake** (amends ADR-0082 §1 for characters). A routed event matching a character's `subscribes` is
  appended to its session as an untrusted entry.
- **Cooldown per subject.**
  - A subject is an alert group's stable label-set hash, or a game scene.
  - After a turn on subject S, further entries on S within the cooldown (default 10 min, **default, owner may
    change**) are queued and coalesced into one entry. They are never dropped.
  - A `resolved` event always passes.

### 6. Reach, and the read identity

- **Everything goes through core, on the lease.**
  - `standingMethods` is `definitionMethods` plus the methods below.
  - Each method is granted per definition through `sidecarTools`, as `PublishEvent` is, and tagged B or C for
    ADR-0086.
- **`SpawnWorker(definition, input, idempotency_key)`** is an in-process `Invoke`.
  - The spawned run is owned by `client:standing:<name>`. It is excluded from the external count and `MaxExternal`.
  - It draws from the character's own sub-pool, `limits.spawns.concurrent` (default 1). The sub-pool is defined even
    when `MAX_DEFINITION_SESSIONS` is 0.
  - `limits.spawns` caps runs per hour and per day.
  - The depth is 1. `WaitForResult` answers only the character's own runs.
- **`ViewLogs`, `QueryMetrics` and catalogue dry-runs are tagged B.** `QueryMetrics` is a read-only PromQL relay over
  `core/internal/promclient`.
- **`OpenIssue` is tagged C**, limited to the definition's `owns.repos`.
- **Cluster reads are a per-definition grant (`cluster.read: true`)** to a dedicated ServiceAccount, bound to a
  read-only ClusterRole in `infra-bootstrap` gitops. The provisioner only names it.
  - The token is mounted in the sidecar only, which serves `kubectl_read` as a B method.
  - The role excludes `secrets`, RBAC objects, and `configmaps` and `pods/log` outside a named namespace list
    (**default, owner may change**).
  - It is granted only after ADR-0086 §6 has removed credentials from pod env.
  - No character gets it by default; game characters never do.
  - The egress lock admits the API server for pods with the grant. The worker container shares the pod's network
    namespace but holds no token, so it reaches anonymous endpoints only.

### 7. Memory

- **One memory repo per character**, as ADR-0086 §5 specifies:
  - owner and fleet turns write through the sidecar's memory tool, one commit per write, tagged with the turn and its
    set;
  - untrusted turns write to the quarantine branch.
- The repo is at `claude-home/standing-<name>-memory` on the shared Longhorn volume.
  - The worker mounts it read-only, so the sidecar is the single writer.
  - The console shows the history, and reverts a commit through a provisioner job under the same lock.

### 8. The game's connection

- **The game has its own connection to core**, and shares UI components with the fleet.
  - It addresses a character through a new `AgentService.PostToCharacter(character, scene, text)`.
  - Core restricts the key per client: the game may use only `scene:*`. Nothing reaches a character's owner channel
    through it.
- **The owner is T0 on that connection only with a verified identity.**
  - The game forwards the player's authentik ID token, and core verifies it itself.
  - The token's subject must map to an `actors.allow` entry of the `ukubi-game` client.
  - Core trusts no actor field the game asserts, which complies with ADR-0056 and ADR-0082 §7 instead of amending
    them.
  - Guests' entries are untrusted. They wake the character as A (amends ADR-0066 §6).
- **Speech is its own channel: `CoreService.Speak(scene, text, face)`**, granted per definition and tagged C.
  - A guest-scene turn is A·C, so it may speak.
  - It publishes `dev.bnei.fleet.speech` carrying the scene and the turn's set. The game shows speech from a
    B-holding turn to the verified owner only.
  - Speech never wakes anything, and the game never republishes it as talk.
  - Core bounds speech at 20 lines per turn (**default, owner may change**).

### 9. Capacity

- **A character pod requests 1Gi and is limited to 2Gi.** That sits within three existing caps:
  - the namespace LimitRange's 4Gi max (`k8s/core.yaml:309-319`);
  - the loader's `maxMemoryMi` (`core/internal/definitions/load.go:59`);
  - `workerMaxMemory` (`provisioner/internal/k8s/pod.go:173`).
- The first build slice applies a definition's `resources` to the worker container. Today they are validated
  (`load.go:658`) and never applied.
- **`MAX_STANDING_SESSIONS` (default 3) is a pool outside the cap of 5.** Workers keep all their slots.
- **Owner to confirm:** 3 × 2Gi at the limit, on two session nodes of 15 and 16 GB. One node carries everything when
  the other is booted elsewhere.
- **One subscription.** Characters use the same OAuth account as every session (ADR-0004). The headroom for
  concurrent streams is unmeasured, which is an open risk.

### 10. Credentials

- ADR-0086 §6 applies: the vault token and the lease sit in a per-pod Secret file, and that PR comes first.
- A character outlives the 12h token cap. The provisioner **replaces** the pod before the cap, under the vault lock,
  through §3's drain.
- It does not rotate in place. That keeps every existing revoke path (the Job's `vault-label` annotation) unchanged.

### 11. Runaway

- **A turn budget per character per calendar day, Europe/Paris** (`limits.turns`).
  - Past it, core holds non-T0 entries; T0 entries still pass.
  - Core files a proposal (ADR-0086 §4) to raise the day's budget. Accepting it sets a core-side override with a 24h
    TTL, not a definition edit.
- **An alert to the owner** when `turns_last_hour × 24 ≥ limits.turns / 2`.

### 12. Runtimes and the game later

- **Stage 1 is Claude Code only.**
- **Later:**
  - a `runtime` field and a Hermes or OpenClaw spike, behind the sidecar contract;
  - multi-character scenes;
  - GitHub scope per session class;
  - gVisor;
  - the agent-sandbox spike for the workload. The Job stays until that spike decides.

## Alternatives considered

- **Several threads in one pod, or one pod per trust tier.** Five doubt rounds of blockers:
  - per-thread trust is unenforceable in one process;
  - a shared vault lets the untrusted tier act;
  - cards answered into a pod that cannot act;
  - illegal sizing.
- **Event-driven single-shot runs instead of a pod that lives on.** Simpler, but it loses the always-on character the
  owner wants and pays a warm on every alert.
- **The game host outside the fleet.** Kept as an option for light talk (the game's voice path). Characters do real
  work, so they stay in the fleet.

## Consequences

- One shape for every character, and every existing invariant holds per pod. Trust is ADR-0086's, per turn, from one
  trigger.
- New surface:
  - lifecycle `standing`, the `standing:` principal and `Start`;
  - the re-warm and its back-off;
  - the cursor and single-trigger delivery;
  - `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`, `Speak` and `PostToCharacter`;
  - the read ServiceAccount;
  - the memory tool.

## The bet

That one session per character, with one trigger per turn, makes trust as simple as it is for a worker. And that
ADR-0086's start-of-turn rule keeps an always-on agent safe enough to self-heal through the catalogue.

## Not doing

- Threads inside a pod.
- Pods per tier.
- Rotating a token in place.
- A retry counter.
- A heartbeat.
- A Deployment.

## Appendix: build slices, in order

Each is its own PR, with its acceptance tests.

1. **`resources` applied.** A definition's `resources` reach the worker container.
   - `TestCreateWorkerPod_ResourcesWithinLimitRange` covers a definition at 1Gi/2Gi.
   - A definition above the 4Gi max is refused at load.
2. **No credential in env** (ADR-0086 §6).
   - The vault token and the lease go in a per-pod Secret file, mounted without a subPath. The Secret is created
     before the Job and adopted with an ownerReference after it.
   - Readers re-read the file per use.
   - `AutomountServiceAccountToken=false` on every pod.
   - Tests: a golden Job holds no credential in env and no SA token mount; the router re-reads the token after a file
     change.
3. **Lifecycle `standing`, the principal, `Start`.**
   - Migration 000027 drops and re-adds `sessions_lifecycle_check` with `standing`, and adds the admin column and
     `restart_after`.
   - Tests:
     - the 000015 `sessions_invoker_owns` and `sessions_thread_key_invoker` CHECKs pass for
       `client:standing:<name>`;
     - the loader refuses a `standing:` client file;
     - `Open`, `Invoke` and a routed proposal are refused on a standing-only definition.
4. **Cursor and single-trigger delivery.**
   - An entry appended while the pod is down is delivered after the re-warm.
   - A second entry during a turn waits for the result.
   - An owner entry jumps the queue and does not interrupt.
5. **Sweeps and re-warm.**
   - The idle, stall and stop-grace sweeps skip standing rows.
   - Every terminal path (`ReportPodEvents`, a failed warm, `CRASHED`) sets `restart_after`, or a NULL counts as due.
   - Stop is excluded from the re-warm.
   - After three failures in an hour the re-warm stops and the owner is alerted, with no counting column.
6. **Counting.**
   - Standing rows count in `MAX_STANDING_SESSIONS` only: never in `MaxLive`, the definition pool, `externalLive`,
     `ownerLive` or `MaxExternal`.
   - A spawned run counts in the character's sub-pool only.
7. **ADR-0086 enforcement for characters.** Methods are tagged B or C.
   - Core refuses a method outside the turn's set (`agent.rule_of_two`).
   - An alert-triggered turn on an A·C character can run a catalogue action and cannot call `ViewLogs`.
8. **Reach.**
   - `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`.
   - The spawn depth is 1, `limits.spawns` is enforced, and `OpenIssue` refuses a repo outside `owns.repos`.
9. **Memory.**
   - The sidecar memory tool, a read-only worker mount, and the quarantine branch.
   - The directory reuses the explicit chmod in `EnsureClaudeHome` (`provisioner/internal/git/git.go:320-330`); no chown claim.
   - An untrusted-turn write lands on the quarantine branch.
   - A replacement pod waits for the old lease before opening the repo.
10. **Pin change, and replacement before the token cap.**
    - Drain, then deadline, then replace.
    - The next `vault-label` is written on the Job before the mint.
    - Revocation tears down at once.
11. **Events.**
    - Bridge delivery, the route, the cooldown, and retiring `/webhook/alertmanager`.
    - Cooldown entries are coalesced and never dropped, `resolved` passes, and the fleet's own alerts never reach a
      character.
12. **The read ServiceAccount.**
    - The SA cannot `get secrets`.
    - It cannot read `configmaps` or `pods/log` outside the named namespaces.
13. **The game's connection and speech.**
    - `PostToCharacter` refuses a non-`scene:*` key.
    - T0 is granted only for a verified authentik ID token whose subject maps to `actors.allow`.
    - Speech from a B-holding turn is marked owner-only, never wakes anything, and is bounded per turn.
14. **Turn budget and alert.**
    - Non-T0 entries are held past the budget.
    - The raise proposal sets a 24h override.
    - The alert formula fires.
