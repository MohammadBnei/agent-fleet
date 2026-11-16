# ADR-0084 — Long-lived characters: one pod per character, a fixed tier for life

- **Status:** Proposed (2026-11-16). This is the final revision after three doubt cycles.
  - The owner chose one pod per character and adopted [ADR-0086](0086-rule-of-two.md) (Rule of Two).
  - He then fixed each character's trust tier for its whole life, and moved alert handling to ADR-0086's single-shot
    first responders.
  - Choices marked **default, owner may change** are the coordinator's defaults.
  - **Owner to confirm** marks a consequence the owner has not yet seen.
  - Mechanisms live in the build-slice appendix.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Depends on:** [ADR-0086](0086-rule-of-two.md) (PR #401) for trust sets, cards and their effect verbs, memory
  quarantine and the per-pod Secret file. ADR-0086 does not depend on this ADR.
- **Would amend:**
  - [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1–§2 and the `CLAUDE.md` locked decision "One pod per
    warm, single-shot": a character's pod is started once and kept up by core (§3).
    - ADR-0048 §2's "no queue", and `CLAUDE.md:56`'s "No queue": core holds a character's undelivered entries and
      delivers them one at a time (§4).
  - [ADR-0058](0058-an-answer-wakes-the-session.md) and `DECISIONS.md` §2 ("appending to a session that has no live
    pod without warming first"): a character tracks delivery per entry, so an entry appended while its pod is down is
    delivered after the re-warm (§4).
  - [ADR-0062](0062-defined-agents.md):
    - §5/§7: the `standing` lifecycle, its ceiling, its pool and the `tier` field (§2, §9);
    - its revocation rule (`definitionWarmable`, `core/internal/dashboard/server.go`): a character is re-pinned in
      place on a definition change (§3).
  - [ADR-0076](0076-definition-sessions-run-in-auto.md): a standing definition's built-ins outside its ceiling are
    denied, not asked (§2).
  - [ADR-0066](0066-agent-service.md) §6: on a character, an untrusted actor's follow-up starts a turn instead of
    being forced to context (§5).
  - [ADR-0082](0082-decision-cards.md) §7: an admin origin may also call `PostToCharacter` (§8).
  - [ADR-0064](0064-definition-pod-profile.md)'s method table: `ViewLogs`, forbidden to definitions, is allowed to a
    trusted character granted it, scoped by namespace (§6).
  - [ADR-0037](0037-thot-is-a-worker-task.md), and `CLAUDE.md:43`, `:57` and `:62` (the only cluster-RBAC holders):
    a granted pod holds a read-only kube identity (§6).
  - [ADR-0045](0045-service-endpoint-roster-direct-dial.md) and `DECISIONS.md` §2 ("a new passthrough RPC on core"):
    `QueryMetrics` is a named exception, because the egress lock blocks a direct dial from a definition pod (§6).
  - [ADR-0017](0017-single-session-planning-pipeline.md) and `DECISIONS.md` §2's "An orchestration framework
    (Hermes, OpenClaw…)": reopened narrowly, later, for a runtime behind the sidecar (§11).
- **Related:** [ADR-0070](0070-headless-invoke.md), [ADR-0071](0071-events-reach-sessions-through-the-bridge.md),
  [ADR-0004](0004-subscription-oauth-not-metered-api.md).

## Context

The fleet runs workers: one pod per session, warmed on a message, torn down when idle. The owner wants **characters**:
named agents that live on. Two are in view:
- an ops lead the owner talks to;
- game hosts that talk with guests.

They need little power: they edit files and memory, and act through ADR-0086's catalogue and cards.

The drafts went through three doubt cycles, and the same failure kept returning. A long-lived context that has read
untrusted text stays untrusted:
- the SDK carries every earlier turn;
- compaction folds that text into a durable summary;
- background processes outlive turns.

So no trust set can change inside a character's life. Alerts need diagnosis and fast action, not continuity, so they
go to ADR-0086's single-shot first responders instead.

## Decision

### 1. Two kinds of agent

- **Workers**, unchanged. That includes ADR-0086's first responders, and the 17 game resident definitions
  (`definitions/game-*.yaml`), which keep today's `Open` path.
- **Characters:** an `AgentDefinition` with `lifecycle: standing`.
  - Each character is one session in one pod, started by the owner and kept up by core until stopped.
  - The pod is a definition pod: placeholders only, behind the egress lock.

### 2. A fixed tier, and a light ceiling

- **`tier: trusted | untrusted`** is declared in the definition. The loader checks it, and it never changes.
  - **Trusted** (the ops lead):
    - It receives only T0 and T1 entries (ADR-0086 §2), and its set is B·C.
    - An alert reaches it only as a brief, a typed context entry written by core from a responder's card
      (ADR-0086 §4). That entry never starts a turn.
  - **Untrusted** (a guest game host):
    - It receives untrusted entries. Its set is A plus the definition's `trust.untrusted` letter, which is C for a
      host: it speaks and files cards.
    - The owner's own lines in a guest scene arrive as entries that do not widen that set.
    - It never holds private memory or cluster reads.
- **The set is stored once** on the session row (ADR-0086 §2). Turns do not change it.
- **A light ceiling, enforced by the loader for `standing`:**
  - built-ins limited to `Read`, `Glob`, `Grep`, `Edit`, `Write` and `TodoWrite`;
  - no `Bash`, `WebFetch` or `WebSearch`;
  - anything outside the ceiling is denied, not asked (amends ADR-0076);
  - the vault serves the Anthropic host, plus GitHub on `owns.repos` for a trusted character only.
  - No Bash means no background process outlives a turn.
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

- **Exempt from the sweeps.** The idle, startup-stall and stop-grace sweeps skip standing rows. The Job's
  `activeDeadlineSeconds` stays at the token cap.
- **Re-warm, a new mechanism.** Today's `ListRewarmRuns` covers single-shot `TERMINATED` only.
  - `restart_after` is set on every terminal path: `ReportPodEvents`, a failed warm, `CRASHED`. A NULL counts as due.
    Stop clears it and is excluded from the selector.
  - The back-off doubles per crash. The count comes from the transcript's own crash entries of the last hour
    (`DECISIONS.md:53-60`: a crash's error lives in the transcript), read at re-warm time.
  - Evictions and preemptions are not crashes.
  - After the third crash in an hour, core stops re-warming and alerts the owner. No column counts anything, so
    ADR-0048 §2 stands for retries.
- **The SDK session rotates; the pod's row does not.**
  - A fresh SDK session (no `resume`) starts daily at 04:00 Europe/Paris, at every pin change, and at Start.
    **Default, owner may change.**
  - An untrusted host starts a fresh SDK session per scene, one scene at a time.
  - Memory is the only carry-over. That bounds the context's growth and what compaction keeps.
- **Re-pinned in place.** On a definition change, core drains:
  - intake stops;
  - the running turn gets its deadline (§4);
  - the Job is deleted, the session is re-pinned to the new SHA, and a new Job is warmed.
  - The downtime is the drain plus one warm: a few seconds after a quiet turn, at most the turn deadline.
  - The same drain replaces the pod before the 12h token cap (§10).
  - Revocation and a vault widening tear down at once, as for any definition.

### 4. Delivery: one turn-starting entry at a time, tracked per entry

- **Per-entry delivery state.** Each turn-starting entry to a character gets `delivered_at` and `result_seq`, written
  by core.
  - Core delivers the lowest-seq undelivered entry that is not held.
  - Holds come from the budget (§10) or the cooldown (§5).
  - This replaces the high-water cursor, `cold_from_seq` and `ExitCursor` for standing rows only. Workers are
    unchanged.
- **One turn at a time.** A message entry waits until the running turn's result. Answers, permission responses and
  Stop always pass, so a turn waiting on a question can be answered and a runaway can be stopped.
- **A per-turn deadline:** 15 min (**default, owner may change**). Past it, core aborts the turn and delivers the
  next entry.
- **Effects carry an idempotency key** of (session, entry seq, method), so a crash that replays a turn never runs an
  effect twice.
- **A poison entry** that crashes its turn three times is marked skipped, and the owner gets an alert. The queue
  behind it moves on.

### 5. Events into a character

- **The dispatcher gains an append path.** A routed event matching a character's `subscribes` is appended to its
  session instead of opening one. The dispatcher reaches definitions only through `Open` today
  (`core/cmd/core/run.go`, `dispatch.New`).
  - An untrusted character takes untrusted events as turn-starting entries. This amends ADR-0066 §6's forced
    context-only follow-up for characters.
  - A trusted character takes only T1 `PublishEvent`s from sessions whose set lacks A, and core-written briefs as
    context.
- **Cooldown per subject.** A subject is a game scene (or, for ADR-0086's responders, an alert's `groupKey` label-set
  hash over `alertname`, `namespace`, `service`).
  - After a turn on subject S, further entries on S within 10 min (**default, owner may change**) are held and
    coalesced into one. They are never dropped.
  - A `resolved` alert passes once it has been firing for at least `group_interval`.
- **The Alertmanager route.** It sends to the bridge with `group_wait 30s`, `group_interval 5m` and
  `repeat_interval 4h`, and feeds ADR-0086's responders.
  - The existing `/webhook/alertmanager` path and its thot proposals and Discord notice stay.
  - On the bridge, an alert that a responder definition subscribes to goes to the responder, not to the dispatcher's
    alert handler, so nothing files twice.
  - Alerts about characters themselves (runaway turns, crash loops) go to the owner only.

### 6. Reach

- **Everything goes through core, on the lease.**
  - `standingMethods` is `definitionMethods` plus the methods below, each granted per definition through
    `sidecarTools`.
  - Each method is tagged B or C for ADR-0086.
  - Sidecar-local tools ask core first (ADR-0086 §2).
- **`SpawnWorker(definition, input, idempotency_key)`** is an in-process `Invoke`, tagged C.
  - The spawned run takes the character's tier. An untrusted character can spawn only a definition with
    `trust.untrusted`.
  - It counts in `MaxDefinition` and `MaxLive`, and in the character's sub-pool, `limits.spawns.concurrent`
    (default 1).
  - Config refuses `MAX_DEFINITION_SESSIONS` below the sum of the characters' sub-pools.
  - `limits.spawns` caps runs per hour and per day. The depth is 1.
  - `WaitForResult` answers only the character's own runs, and is tagged B when the run's set holds B.
- **`ViewLogs` and `QueryMetrics` are tagged B, for trusted characters only.**
  - `ViewLogs` is scoped to the namespace list below.
  - `QueryMetrics` is a read-only PromQL relay over core's existing `promclient`.
- **`OpenIssue` is tagged C**, limited to `owns.repos`.
- **Cluster reads are a per-definition grant (`cluster.read: true`).**
  - Only trusted characters and ADR-0086's responders can hold it.
  - It goes to a dedicated ServiceAccount whose token is mounted in the sidecar only, after ADR-0086 §6 has cleaned
    pod env.
  - **The role is an explicit allowlist in `infra-bootstrap`:**
    - `get`/`list`/`watch` on `pods`, `events`, `nodes`, `services`, `persistentvolumeclaims`;
    - `apps/deployments`, `statefulsets`, `daemonsets`, `replicasets`;
    - `batch/jobs`, `cronjobs`;
    - `argoproj.io/applications`.
  - `pods/log` comes only through Roles in a named namespace list (**default, owner may change**).
  - Never `secrets`, `configmaps`, RBAC objects, `*/exec`, `*/proxy` or `nodes/proxy`.
  - The egress lock admits the API server for granted pods.

### 7. Memory

ADR-0086 §5 applies.
- A trusted character's memory repo is `claude-home/agent-<name>-memory`. The sidecar is its single writer, and the
  worker's mount is read-only.
- An untrusted character writes only to `claude-home/agent-<name>-quarantine`, which no trusted session mounts.
- Promote and revert go through the sidecar tool. A replacement pod's sidecar opens the repo only after the old Job
  has terminated, which the drain (§3) guarantees.

### 8. The game

- **Owner talk from the game goes from the browser straight to core**, under core's own OIDC session, from an admin
  origin (`FLEET_ADMIN_ORIGINS`).
  - That path calls `AgentService.PostToCharacter(character, text)` (amends ADR-0082 §7's method list) and reaches a
    trusted character as T0.
  - The game server never carries the owner's identity.
- **The game server's connection carries guests only.** Its `PostToCharacter(character, scene, text)` accepts only
  `scene:*` keys and only untrusted characters, and every entry it posts is untrusted.
- **Speech, `CoreService.Speak(text, face)`**, is granted per definition and tagged C. Core binds it to the turn's
  trigger scene; the agent does not choose the scene.
  - An untrusted host's speech goes out on the bridge as `dev.bnei.fleet.speech`, for the game server. The
    dispatcher never delivers it to a definition.
  - A trusted character's speech goes only to the owner's browser, through core's stream on the admin origin.
  - Core bounds speech at 20 lines per turn (**default, owner may change**).

### 9. Capacity

- **A character pod requests 1Gi and is limited to 2Gi**, plus its sidecar (64Mi request, 512Mi limit,
  `provisioner/internal/k8s/pod.go`).
  - That sits within the LimitRange max of 4Gi (`k8s/core.yaml:309-319`), `maxMemoryMi` (`load.go:59`) and
    `workerMaxMemory` (`pod.go:173`).
  - The first slice applies a definition's `resources` to the worker; today they are validated (`load.go:658`) and
    never applied.
- **`MAX_STANDING_SESSIONS` is a pool outside the cap of 5.** Spawns stay inside the definition pool.
- **Owner to confirm, recommended 2 characters (an ops lead and one game host).**
  - At the limit: 2 × 2.5Gi = 5Gi, beside 5 workers × 4Gi = 20Gi. That makes 25Gi against 31GB of session nodes, or
    15GB with one node out.
  - Characters should be **Guaranteed QoS** (request = limit = 1.5Gi), so memory pressure evicts workers first.
- **One subscription.** Characters share the OAuth account (ADR-0004). The headroom for concurrent streams is
  unmeasured, which is an open risk.

### 10. Credentials and runaway

- ADR-0086 §6 applies.
- A character outlives the 12h token cap, so the provisioner replaces the pod through §3's drain before the cap,
  under the vault lock. The next `vault-label` is written on the Job before the mint.
- **`limits.turnsPerDay`**, per character per calendar day, Europe/Paris.
  - Past it, non-T0 entries are held.
  - Core files a card whose effect verb `raise_budget` sets a core-side override with a 24h TTL.
- **An alert to the owner** when `turns_last_hour × 24 ≥ turnsPerDay / 2`.

### 11. Later

- A `runtime` field and a Hermes or OpenClaw spike behind the sidecar.
- Multi-character scenes.
- GitHub scope per session class.
- gVisor.
- The agent-sandbox spike for the workload. The Job stays until it decides.

## Alternatives considered

- **Several threads in one pod, or one pod per tier.** Five rounds of blockers.
- **A per-turn tier inside one character.** The context carries the earlier turns' text, so it recreates the lethal
  trifecta. This was doubt cycle 3's B1.
- **An ops character that takes alerts.** Replaced by ADR-0086's single-shot responders. Each alert gets a fresh
  context, and the character hears a brief.
- **The game host outside the fleet.** Kept for light talk (the game's voice path).

## Consequences

- Each character's trust is fixed and visible. No turn changes it, and no untrusted text reaches a trusted context.
- New surface:
  - lifecycle `standing`, `tier`, the `standing:` principal, `Start` and Stop;
  - the re-warm and per-entry delivery;
  - SDK rotation;
  - `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`, `Speak` and `PostToCharacter`;
  - the read ServiceAccount;
  - the memory tool.

## The bet

That a fixed tier per character, a light ceiling with no Bash, and alerts handled by fresh runs make an always-on
agent as easy to reason about as a worker.

## Not doing

- Threads inside a pod.
- Pods per tier.
- Per-turn tiers.
- Bash in characters.
- Rotating a token in place.
- A retry counter.
- A heartbeat.
- A Deployment.

## Appendix: build slices, in order

Each slice is its own PR, with its acceptance tests. Migration numbers are the next free ones at build time.

1. **`resources` applied.**
   - `TestCreateWorkerPod_ResourcesWithinLimitRange` covers 1Gi/2Gi and Guaranteed QoS.
   - A definition above the 4Gi max is refused at load.
2. **No credential in env** (ADR-0086 §6).
   - The Secret file and automount off.
   - Golden Jobs check env and mounts.
3. **`standing`, `tier`, the principal, `Start` and Stop.**
   - The lifecycle CHECK is dropped and re-added in a new migration.
   - The 000015 CHECKs pass for `client:standing:<name>`.
   - The loader refuses a `standing:` client file, a missing `tier`, and Bash in a standing ceiling.
   - `Open`, `Invoke` and routed proposals are refused.
4. **Per-entry delivery.**
   - An entry appended while the pod is down is delivered after the re-warm.
   - A held entry does not block later ones.
   - Answers and Stop pass during a turn.
   - The turn deadline aborts a turn.
   - A replayed turn does not repeat an effect (idempotency key).
   - A poison entry is skipped after three crashes.
5. **Sweeps, re-warm, rotation and re-pin.**
   - The sweeps skip standing rows.
   - Every terminal path sets `restart_after`.
   - An eviction is not counted as a crash.
   - Three crashes stop the re-warm.
   - Daily SDK rotation.
   - A pin change re-pins in place.
6. **Counting.**
   - Standing rows count only in `MAX_STANDING_SESSIONS`.
   - Spawns count in `MaxDefinition`, `MaxLive` and the sub-pool.
   - The config check on the pool sum.
7. **Tier and set enforcement.**
   - An untrusted event never reaches a trusted character.
   - A T0 entry does not widen an untrusted character's set.
   - Sidecar-local B and C tools are refused outside the set.
8. **Reach.**
   - `SpawnWorker` takes the character's tier.
   - `OpenIssue` stays within `owns.repos`.
   - `ViewLogs` is scoped by namespace.
9. **Memory.**
   - The worker's mount is read-only.
   - An untrusted character's write lands in quarantine.
   - The replacement sidecar waits for the old Job.
10. **Token cap and revocation.**
    - Replacement before the cap.
    - The label is written before the mint.
    - Revocation tears down at once.
11. **Events.**
    - The dispatcher append path.
    - Cooldown coalescing.
    - The resolved debounce.
    - Bridge alerts go to responders without a second filing.
    - Alerts about characters reach only the owner.
12. **The read ServiceAccount.**
    - The allowlist.
    - No `secrets`, `configmaps`, `*/exec` or `*/proxy`.
    - `pods/log` only in named namespaces.
13. **The game.**
    - `PostToCharacter` from an admin origin is T0.
    - From the game server it is untrusted, `scene:*` only, untrusted characters only.
    - `Speak` is bound to the trigger scene and bounded.
    - A trusted character's speech never reaches the bridge.
14. **Budget and alert.**
    - Holds past the budget.
    - `raise_budget` sets a 24h override.
    - The alert formula fires.
