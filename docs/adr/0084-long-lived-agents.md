# ADR-0084 — Long-lived characters: one trusted pod per character, kept up by core

- **Status:** Proposed (2026-11-16). Final revision.
  - The owner chose one pod per character and adopted [ADR-0086](0086-rule-of-two.md) (Rule of Two).
  - Guests never reach the fleet. Every character is the owner's, and so trusted.
  - Alerts go to ADR-0086's single-shot first responders.
  - Choices marked **default, owner may change** are the coordinator's defaults.
  - **Owner to confirm** marks a consequence the owner has not yet seen.
  - Mechanisms live in the build-slice appendix.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Depends on:** [ADR-0086](0086-rule-of-two.md) (PR #401) for trust sets, cards, memory and the per-pod Secret
  file. ADR-0086 does not depend on this ADR.
- **Would amend:**
  - [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1–§2 and the `CLAUDE.md` locked decision "One pod per
    warm, single-shot": a character's pod is started once and kept up by core (§3).
  - [ADR-0058](0058-an-answer-wakes-the-session.md) and `DECISIONS.md` §2 ("appending to a session that has no live
    pod without warming first"): a character resumes from its last result's cursor, so an entry appended while its
    pod is down is delivered after the re-warm (§4).
  - [ADR-0062](0062-defined-agents.md):
    - §5/§7: the `standing` lifecycle, its pool and `spawns.allow` (§2, §6, §9);
    - its revocation rule (`definitionWarmable`, `core/internal/dashboard/server.go`): a character is re-pinned in
      place on a definition change that widens nothing (§3).
  - [ADR-0082](0082-decision-cards.md) §7: an admin origin may also call `PostToCharacter` and receive `Speak` (§7).
  - [ADR-0064](0064-definition-pod-profile.md)'s method table: `ViewLogs`, forbidden to definitions, is allowed to a
    character granted it, scoped by namespace (§6).
  - [ADR-0037](0037-thot-is-a-worker-task.md), and `CLAUDE.md:43`, `:57` and `:62` (the only cluster-RBAC holders):
    a granted pod holds a read-only kube identity (§6).
  - [ADR-0045](0045-service-endpoint-roster-direct-dial.md) and `DECISIONS.md` §2 ("a new passthrough RPC on core"):
    `QueryMetrics` is a named exception, because the egress lock blocks a direct dial from a definition pod (§6).
  - [ADR-0017](0017-single-session-planning-pipeline.md) and `DECISIONS.md` §2's "An orchestration framework
    (Hermes, OpenClaw…)": reopened narrowly, later, for a runtime behind the sidecar (§10).
- **Related:** [ADR-0070](0070-headless-invoke.md), [ADR-0071](0071-events-reach-sessions-through-the-bridge.md),
  [ADR-0004](0004-subscription-oauth-not-metered-api.md).

## Context

The fleet runs workers: one pod per session, warmed on a message, torn down when idle. The owner wants **characters**:
named agents that live on, that he talks to, and that keep their own memory. Examples are an ops lead and the game's
world and village residents when he talks to them in the game. They edit files and memory, commit and push with
`Bash(git …)` as dev-notes does, and act through ADR-0086's catalogue and cards.

Three doubt cycles on earlier drafts kept finding the same failure. A long-lived context that has read untrusted text
stays untrusted: the SDK carries every earlier turn, and compaction folds that text into a durable summary. Two owner
decisions remove the problem at its source:
- guests use the game's own voice path and never reach the fleet ("pre conceived direct LLM call, 0 access to repos
  or anything");
- alerts go to fresh single-shot responders.

What is left is a trusted agent that lives on.

## Decision

### 1. Two kinds of agent

- **Workers**, unchanged. That includes ADR-0086's first responders and the 17 game resident definitions
  (`definitions/game-*.yaml`), which keep today's `Open` path.
- **Characters:** an `AgentDefinition` with `lifecycle: standing`.
  - Each character is one session in one pod, started by the owner and kept up by core until stopped.
  - The pod is a definition pod: placeholders only, behind the egress lock.

### 2. Trusted, for life

- **A character receives only T0 and T1 entries** (ADR-0086 §2), and its stored set is B·C. The loader refuses a
  standing definition that declares `trust.untrusted` or subscribes to an untrusted event type.
- **Its ceiling is an ordinary definition ceiling** (ADR-0062, ADR-0076): `tools.allow` may include Bash, for example
  dev-notes' `Bash(git add|commit|pull --rebase|push:*)` (`definitions/dev-notes.yaml:25-26`).
- **A character is a principal of its own, `standing:<name>`.**
  - The loader reserves the prefix and refuses it in client files.
  - The row is owned by `client:standing:<name>`, which satisfies migration 000015's `sessions_invoker_owns` and
    `sessions_thread_key_invoker` CHECKs.
  - The admin who started it is kept in its own column.
- **Only `Start` opens it.** Start is an admin console call. Core refuses `Open`, `Invoke` and a routed proposal for
  a standing definition (`agent.standing_only`).
  - Stop marks the row stopped. Events for a stopped character file a card instead of being appended.
  - Start on a stopped character resumes the row with a fresh SDK session (§3), and memory carries over.

### 3. Lifecycle: started once, kept up by core

- **Exempt from the idle and stop-grace sweeps.**
  - The startup-stall sweep does not tear a character down. It alerts the owner instead.
  - The Job's `activeDeadlineSeconds` stays at the token cap.
- **Re-warm, a new mechanism.** Today's `ListRewarmRuns` covers single-shot `TERMINATED` only.
  - `restart_after` is set on every terminal path: `ReportPodEvents`, a failed warm, `CRASHED`. A NULL counts as due.
    Stop clears it and is excluded from the selector.
  - **On every `CRASHED` path, core appends a crash entry** to the transcript with the pod's reason. Evicted and
    Preempted get none.
  - The back-off doubles per crash entry of the last hour, read at re-warm time.
  - After the third crash in an hour, core stops re-warming and alerts the owner. No column counts anything, so
    ADR-0048 §2 stands for retries.
- **Storage.** **Owner to confirm** (recommended):
  - the character's session PVC is a Longhorn RWO volume rather than local-path, so its working tree survives a node
    loss and the pod can be scheduled on either node;
  - the alternative keeps local-path, accepting that a character is pinned to one node and goes down with it.
- **The SDK session rotates; the row does not.**
  - A fresh SDK session (no `resume`) starts daily at 04:00 Europe/Paris, at every re-pin and at Start.
    **Default, owner may change.**
  - Memory is the only carry-over. That bounds the context's growth and what compaction keeps.
- **Re-pinned in place, only when nothing widens.** On a definition change, core drains:
  - intake stops;
  - the running turn gets its deadline (§4);
  - the Job is deleted, the session is re-pinned to the new SHA, and a new Job is warmed.
  - The downtime is the drain plus one warm: a few seconds after a quiet turn, at most the turn deadline.
  - Re-pin is refused, and the owner must Stop and Start, when the change widens the ceiling, the vault, `cluster.read`
    or `spawns.allow`.
  - A narrowing, a revocation or a vault widening tears down at once, as for any definition.
  - The same drain replaces the pod before the 12h token cap (§9).

### 4. Delivery: the worker's cursor, in strict order

- **The high-water cursor, unchanged from workers.** Entries are delivered in strict seq order. There are no
  priority jumps.
- **The cursor survives the pod.** A character resumes from its last result's `consumedSeq`, as a single-shot run's
  `ExitCursor` does (`core/internal/sessions/store.go`). It never resumes from `LatestSeq` at warm, so entries appended
  while the pod is down are delivered after the re-warm.
- **One turn at a time.**
  - A message entry waits until the running turn's result.
  - Answers, permission responses and Stop always pass, so a turn waiting on a question can be answered and a
    runaway can be stopped.
  - A per-turn deadline of 15 min (**default, owner may change**) aborts a turn, and the next entry is delivered.
- **The budget hold (§9) holds the head of the queue only.** It never skips an entry, so order is kept.
- **Effects are idempotent.**
  - Every effect method carries a key of (session, entry seq, per-turn call ordinal).
  - Once any effect of an entry has run, a crash marks that entry done: core advances the cursor past it with a
    crash result, rather than replaying the turn.
- **A poison entry** that crashes its turn three times before any effect is marked done the same way, and the owner
  gets an alert.

### 5. What reaches a character

- **The owner's messages**: console, the game's admin origin (§7), schedules.
- **T1 entries:** `PublishEvent` from sessions whose set lacks A, appended by the dispatcher instead of opening a
  session. The dispatcher reaches definitions only through `Open` today (`core/cmd/core/run.go`, `dispatch.New`).
- **Its spawned runs' outcomes.** Core appends a turn-starting entry when a spawned run ends (§6).
- **Alerts never.** A character learns about them through a B read method, `ListResponderCards(since)`.
  - It returns ADR-0086 responder cards as core-resolved typed fields only: the alert name, enums, numbers, object
    ids, and the outcome (the catalogue action run, the card's state).
  - It never returns the agent claim, free label or annotation values, or alert text.
  - The character calls it when the owner asks, or on a schedule the owner sets. Nothing is pushed into its context.

### 6. Reach

- **Everything goes through core, on the lease.**
  - `standingMethods` is `definitionMethods` plus the methods below, each granted per definition through
    `sidecarTools` and tagged B or C for ADR-0086.
  - Sidecar-local tools ask core first (ADR-0086 §2).
- **`SpawnWorker(definition, input, idempotency_key)`** is an in-process `Invoke`, tagged C.
  - The definition must be on the character's `spawns.allow` list, which replaces the `invokers` check for spawns.
  - The spawned run is T1.
  - It counts in `MaxDefinition` and `MaxLive`, and in the character's sub-pool, `limits.spawns.concurrent`
    (default 1). Config refuses `MAX_DEFINITION_SESSIONS` below the sum of the characters' sub-pools.
  - `limits.spawns` caps runs per hour and per day. The depth is 1.
  - `WaitForResult` answers only the character's own runs. Core also appends a turn-starting entry when a run ends.
- **`ViewLogs` and `QueryMetrics` are tagged B.**
  - `ViewLogs` is scoped to the namespace list below.
  - `QueryMetrics` is a read-only PromQL relay over core's existing `promclient`.
- **`OpenIssue` is tagged C**, limited to `owns.repos`.
- **Cluster reads are a per-definition grant (`cluster.read: true`).**
  - It goes to a dedicated ServiceAccount whose token is mounted in the sidecar only, after ADR-0086 §6 has cleaned
    pod env.
  - **The role is an explicit allowlist in `infra-bootstrap`:**
    - `get`/`list`/`watch` on `pods`, `events`, `nodes`, `services`, `persistentvolumeclaims`;
    - `apps/deployments`, `statefulsets`, `daemonsets`, `replicasets`;
    - `batch/jobs`, `cronjobs`;
    - `argoproj.io/applications`.
  - `pods/log` comes only through Roles in a named namespace list (**owner to confirm**).
  - Never `secrets`, `configmaps`, RBAC objects, `*/exec`, `*/proxy` or `nodes/proxy`.
  - The egress lock admits the API server for granted pods.

### 7. The game, for the owner only

- **Owner talk from the game goes from the browser straight to core**, under core's own OIDC session, from an admin
  origin (`FLEET_ADMIN_ORIGINS`). That path calls `AgentService.PostToCharacter(character, text)`, as T0.
- **The game server has no connection to characters.** Guests use the game's voice path.
- **`CoreService.Speak(text, face)`**, granted per definition and tagged C, sends a character's line to the owner's
  browser on that admin origin. It never goes on the bridge and never wakes anything.
  - Core bounds it at 20 lines per turn (**default, owner may change**).
  - A character that does not appear in the game has no `Speak` grant.

### 8. Memory

ADR-0086 §5 applies.
- The repo is `claude-home/agent-<name>-memory`. The sidecar is its single writer, and the worker's mount is
  read-only.
- Promote and revert go through the sidecar tool. A replacement pod's sidecar opens the repo only after the old Job
  has terminated, which the drain (§3) guarantees.

### 9. Capacity, credentials and runaway

- **Guaranteed QoS.** **Owner to confirm.**
  - The worker requests and is limited to 1.5Gi memory and 500m CPU.
  - The sidecar requests and is limited to 256Mi and 100m. Its limit today is 512Mi (`provisioner/internal/k8s/pod.go`).
  - That sits within the LimitRange max of 4Gi (`k8s/core.yaml:309-319`), `maxMemoryMi` (`load.go:59`) and
    `workerMaxMemory` (`pod.go:173`).
  - The first slice applies a definition's `resources` to the worker; today they are validated (`load.go:658`) and
    never applied.
- **`MAX_STANDING_SESSIONS` is a pool outside the cap of 5.** Spawns stay inside the definition pool.
- **Owner to confirm: 2 characters.**
  - 2 × 1.75Gi = 3.5Gi, requested and limited.
  - Beside that, 5 workers × 4Gi = 20Gi at the limit (1Gi requested each).
  - 23.5Gi of limits against 31GB of session nodes, or 15GB with one node out. Guaranteed characters are evicted
    last.
- **One subscription.** Characters share the OAuth account (ADR-0004). The headroom for concurrent streams is
  unmeasured, which is an open risk.
- **Credentials.** ADR-0086 §6 applies. A character outlives the 12h token cap, so the provisioner replaces the pod
  through §3's drain before the cap, under the vault lock. The next `vault-label` is written on the Job before the mint.
- **`limits.turnsPerDay`**, per character per calendar day, Europe/Paris.
  - Past it, the head of the queue is held unless it is T0.
  - Core files a card whose effect verb `raise_budget` sets a core-side override with a 24h TTL.
- **An alert to the owner** when `turns_last_hour × 24 ≥ turnsPerDay / 2`.

### 10. Later

- A `runtime` field and a Hermes or OpenClaw spike behind the sidecar.
- Multi-character scenes.
- GitHub scope per session class.
- gVisor.
- The agent-sandbox spike for the workload. The Job stays until it decides.

## Alternatives considered

- **Several threads in one pod, or one pod per tier.** Five rounds of blockers.
- **Untrusted characters (guest hosts).** Their context carries guest text for good. Dropped: guests never reach the
  fleet.
- **A per-turn tier inside one character.** It recreates the lethal trifecta through the context. Dropped.
- **An ops character that takes alerts.** Replaced by ADR-0086's responders, with the character reading typed
  outcomes.
- **No Bash in characters.** Needless once every character is trusted, and "very, very limiting" (owner).

## Consequences

- A character is a trusted worker that lives on: one session, one pod, the worker's cursor, and Bash within its
  ceiling.
- New surface:
  - lifecycle `standing`, the `standing:` principal, `Start` and Stop;
  - the re-warm, crash entries and SDK rotation;
  - `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`, `ListResponderCards`, `Speak` and
    `PostToCharacter`;
  - the read ServiceAccount;
  - the memory tool.

## The bet

That with guests outside the fleet and alerts in fresh runs, a long-lived character only ever sees trusted input, so
it can be as capable as the owner's own sessions.

## Not doing

- Untrusted characters.
- Threads inside a pod.
- Per-turn tiers.
- Priority delivery.
- Rotating a token in place.
- A retry counter.
- A heartbeat.
- A Deployment.

## Appendix: build slices, in order

Each slice is its own PR, with its acceptance tests. Migration numbers are the next free ones at build time.

1. **`resources` applied, Guaranteed QoS.**
   - `TestCreateWorkerPod_ResourcesWithinLimitRange` covers requests equal to limits for the worker and the sidecar.
   - A definition above the 4Gi max is refused at load.
2. **No credential in env** (ADR-0086 §6).
   - The Secret file and automount off.
   - Golden Jobs check env and mounts.
3. **`standing`, the principal, `Start` and Stop.**
   - The lifecycle CHECK is dropped and re-added in a new migration.
   - The 000015 CHECKs pass for `client:standing:<name>`.
   - The loader refuses a `standing:` client file, and `trust.untrusted` or an untrusted subscription on a standing
     definition.
   - `Open`, `Invoke` and routed proposals are refused.
4. **Cursor and delivery.**
   - An entry appended while the pod is down is delivered after the re-warm, in seq order.
   - Answers and Stop pass during a turn.
   - The turn deadline aborts a turn.
   - An entry whose effect ran is not replayed after a crash.
   - A poison entry is marked done after three crashes.
   - The budget holds only the head.
5. **Sweeps, re-warm, rotation and re-pin.**
   - The idle and stop-grace sweeps skip standing rows. A startup stall alerts.
   - Every terminal path sets `restart_after`.
   - A crash entry is appended on every `CRASHED` path, and none for Evicted or Preempted.
   - Three crashes stop the re-warm.
   - Daily SDK rotation.
   - Re-pin in place, and its refusal on a widening.
6. **Counting.**
   - Standing rows count only in `MAX_STANDING_SESSIONS`.
   - Spawns count in `MaxDefinition`, `MaxLive` and the sub-pool.
   - The config check on the pool sum.
7. **Set enforcement.**
   - An untrusted event never reaches a character.
   - Sidecar-local B and C tools are refused outside the set.
8. **Reach.**
   - `SpawnWorker` only for `spawns.allow`; the run's end appends an entry.
   - `OpenIssue` stays within `owns.repos`.
   - `ViewLogs` is scoped by namespace.
   - `ListResponderCards` returns typed fields only.
9. **Memory.**
   - The worker's mount is read-only.
   - The replacement sidecar waits for the old Job.
10. **Token cap and revocation.**
    - Replacement before the cap.
    - The label is written before the mint.
    - Revocation tears down at once.
11. **The read ServiceAccount.**
    - The allowlist.
    - No `secrets`, `configmaps`, `*/exec` or `*/proxy`.
    - `pods/log` only in named namespaces.
12. **The game.**
    - `PostToCharacter` from an admin origin is T0.
    - `Speak` reaches only the owner's browser, never the bridge, and is bounded.
13. **Budget and alert.**
    - `raise_budget` sets a 24h override.
    - The alert formula fires.
