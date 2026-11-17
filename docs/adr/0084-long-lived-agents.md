# ADR-0084 — Long-lived characters: one trusted pod per character, kept up by core

- **Status:** Accepted (owner, 2026-11-16)
  - The owner chose one pod per character and adopted [ADR-0086](0086-rule-of-two.md) (Rule of Two).
  - Guests never reach the fleet. Every character is the owner's, and so trusted.
  - Alerts go to ADR-0086's single-shot first responders.
  - Choices marked **default, owner may change** are the coordinator's defaults.
  - **Owner to confirm** marks a consequence the owner has not yet seen.
  - Mechanisms live in the build-slice appendix.
- **Date:** 2026-11-16
- **Built:** appendix slice 3: the `standing` lifecycle, the `standing:<name>` principal, and an admin's Start and Stop
  (`DashboardService.StartStanding`, `StopStanding`). Start makes the row and provisions nothing; the first message
  warms it, until slice 5's re-warm.
- **Built:** appendix slice 6: counting. Standing rows count only in `MAX_STANDING_SESSIONS` (default 2) and leave
  `MaxLive`, `MaxDefinition` and a definition's concurrency. Not in this slice: the spawn sub-pool and the pool-sum
  config check, which need `limits.spawns` and so move to slice 8.
- **Built:** appendix slice 5: sweeps, re-warm and re-pin (migration 000033: `sessions.restart_after`, `drain_started_at`, `warm_failures`).
  - The idle sweep skips a character; the stop-grace sweep skips it unless the owner stopped it; a startup stall alerts
    once and tears nothing down.
  - `SetPodPhase` stamps `restart_after` on TERMINATED and CRASHED, and a failed warm stamps it. Core's loop warms a
    started character with no pod once `restart_after` plus the back-off has passed.
  - A CRASHED report appends a crash entry (a `fleet` system entry, keyed by the Job UID, which a re-warm changes
    while the Job name stays) to the thread whose turn was running, or main. The provisioner reads the pod's
    Evicted or Preempted reason (pod status or DisruptionTarget condition) into `PodEvent.termination_reason`;
    such a loss adds no crash entry and counts no poison crash, and still closes a head whose effect ran. The
    back-off is 10 s doubled per crash entry of the last hour plus per failed warm since the last good one, capped at
    an hour. A warm that fails (a provisioner or a core-side refusal) counts in `warm_failures` instead of a crash
    entry. The third crash entry stops the re-warm and alerts the owner; the stop lapses by itself once the entries
    are an hour old, so a crash-looping character is retried and alerted again about hourly.
  - Re-pin in place (`Loop.repinStanding`): a change that widens `tools.allow`, a deny, `sidecarTools`,
    `cluster.read` or the vault is refused with one alert (a vault widening also takes the pod down). A narrowing or
    a revocation takes the pod down at once. Anything else drains: intake of new turns stops on every thread, the
    running turn gets its 15-minute deadline, then the Job is deleted, the row re-pinned and a new Job warmed. A drain
    that ends without a new pod (the head widens, reverts or cannot be read) ends the drain; entries it held back are
    delivered to the live pod. A character whose pin is revoked or widened is not re-warmed until Stop and Start.
    Alerts are remembered per session and kind: the stall alert clears when output arrives or the pod changes.
  - Not in this slice: `spawns.allow` in the widening test (the spec has none yet) and the 12 h token-cap
    replacement.
- **Built:** appendix slice 4: threads and delivery per thread (migration 000031).
  - `transcript.thread_key` and `session_threads` (cursor, SDK session id, last activity, effect and crash marks). The
    owner's message to a new key opens a thread (`PostMessage.thread_key`); `CloseStandingThread` or 7 idle days close
    it. Main never closes and keeps its SDK session id in `sessions.agent_session_id`.
  - Core delivers per thread from each thread's own cursor, the last result's `consumedSeq`, never from `LatestSeq`
    (`StreamHumanMessages`, `sessions.ResumeFrom`). A closed thread gets only the abort that ends its SDK session.
  - The worker runs one SDK session per thread and one running thread per pod. A waiting message holds its thread's
    cursor back. Answers, permission responses and Stop pass. A turn passes its 15-minute deadline and is interrupted.
  - The sidecar files every call under the running turn's thread (`x-fleet-thread`, set by `POST /turn`), and keys
    `publish_event`, `draft_card`, `submit_review` and `memory_write` by (session, thread, entry seq, call ordinal).
    On a standing pod an effect outside a turn is refused, and the agent cannot pass a key of its own shape.
  - A CRASHED pod settles a thread's head entry when an effect of it ran or it crashed three times in a row: core
    appends a crash result, which moves the cursor, and a poison entry alerts the owner.
  - Not in this slice: the budget hold (slice 13 owns the budget), the crash entry for every thread and the re-warm
    itself (slice 5).
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
- **Storage.** The character's session PVC is a Longhorn RWO volume rather than local-path, so its working tree
  survives a node loss and the pod can be scheduled on either node.
- **Threads carry the SDK sessions (§4).** There is no scheduled rotation. A long thread relies on Claude Code's
  auto-compaction.
- **Re-pinned in place, only when nothing widens.** On a definition change, core drains:
  - intake stops on every thread;
  - each running turn gets its deadline (§4);
  - the Job is deleted, the session is re-pinned to the new SHA, and a new Job is warmed.
  - The downtime is the drain plus one warm: a few seconds after a quiet turn, at most the turn deadline.
  - Re-pin is refused, and the owner must Stop and Start, when the change widens the ceiling, the vault, `cluster.read`
    or `spawns.allow`.
  - A narrowing, a revocation or a vault widening tears down at once, as for any definition.
  - The same drain replaces the pod before the 12h token cap (§9).

### 4. Threads, and delivery per thread

- **Hermes-style threads inside one character.** A thread is a topic the owner opens.
  - One pod per character; one SDK session per thread, each with its own SDK session id.
  - Transcript entries of standing rows carry a `thread_key` (migration). The owner's admin thread is `main`.
  - A thread closes when the owner closes it, or after 7 days idle. Closing drops its SDK session id. Its transcript
    stays.
  - Memory is shared across threads through the sidecar, the single writer (§8).
- **Why this is safe now.** A character receives only T0 and T1 entries (§2), so every thread is T0/T1 and every
  thread holds the same set.
  - Cycle 1's B2 objection was that a prompt-injected thread could claim another thread's trust through Bash and the
    shared loopback. It no longer applies: no thread holds anything another lacks.
  - The isolation between threads is for context, not for trust.
- **Running threads per pod: 1.** The owner asked for 2, but they do not fit in 1.5Gi.
  - The measured RSS of a worker at an OOM kill was Claude Code 427Mi and the worker's Bun 90Mi
    (`provisioner/internal/k8s/pod.go`, the OOM comment). Two CLI processes therefore start near 0.95Gi before any
    growth.
  - A long thread's heap grows until compaction, and `memory.oom.group` kills every process in the container at the
    limit, so one thread's growth would cut both threads' turns.
  - 2 running threads need the worker at 2Gi. **Owner to confirm.** The capacity sum (§9) then becomes
    2 × 2.25Gi = 4.5Gi.
  - Other threads wait, each at its own head.
- **A high-water cursor per thread, in strict seq order**, unchanged from workers within a thread. There are no
  priority jumps.
- **The cursors survive the pod.** Each thread resumes from its last result's `consumedSeq`, as a single-shot run's
  `ExitCursor` does (`core/internal/sessions/store.go`). It never resumes from `LatestSeq` at warm, so entries appended
  while the pod is down are delivered after the re-warm.
- **One turn at a time per thread.**
  - A message entry waits until its thread's running turn ends.
  - Answers, permission responses and Stop always pass, so a turn waiting on a question can be answered and a
    runaway can be stopped.
  - A per-turn deadline of 15 min aborts a turn, and the thread's next entry is delivered.
- **The budget hold (§9) is per character, across threads.** It holds each thread at its head only, and never skips
  an entry.
- **Effects are idempotent.**
  - Every effect method carries a key of (session, thread_key, entry seq, per-turn call ordinal).
  - Once any effect of an entry has run, a crash marks that entry done in its thread: core advances that thread's
    cursor past it with a crash result, rather than replaying the turn.
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
  - `pods/log` comes only through Roles in a named namespace list (**owner to confirm**: the list).
  - Never `secrets`, `configmaps`, RBAC objects, `*/exec`, `*/proxy` or `nodes/proxy`.
  - The egress lock admits the API server for granted pods.

### 7. The game, for the owner only

- **Owner talk from the game goes from the browser straight to core**, under core's own OIDC session, from an admin
  origin (`FLEET_ADMIN_ORIGINS`). That path calls `AgentService.PostToCharacter(character, text)`, as T0.
- **The game server has no connection to characters.** Guests use the game's voice path.
- **`CoreService.Speak(text, face)`**, granted per definition and tagged C, sends a character's line to the owner's
  browser on that admin origin. It never goes on the bridge and never wakes anything.
  - Core bounds it at 20 lines per turn.
  - A character that does not appear in the game has no `Speak` grant.

### 8. Memory

ADR-0086 §5 applies.
- The repo is `claude-home/agent-<name>-memory`. The sidecar is its single writer, and the worker's mount is
  read-only.
- Promote and revert go through the sidecar tool. A replacement pod's sidecar opens the repo only after the old Job
  has terminated, which the drain (§3) guarantees.

### 9. Capacity, credentials and runaway

- **Guaranteed QoS.**
  - The worker requests and is limited to 1.5Gi memory and 500m CPU, which runs 1 thread at a time (§4).
  - The sidecar requests and is limited to 256Mi and 100m. Its limit today is 512Mi (`provisioner/internal/k8s/pod.go`).
  - That sits within the LimitRange max of 4Gi (`k8s/core.yaml:309-319`), `maxMemoryMi` (`load.go:59`) and
    `workerMaxMemory` (`pod.go:173`).
  - The first slice applies a definition's `resources` to the worker; today they are validated (`load.go:658`) and
    never applied.
- **`MAX_STANDING_SESSIONS` is a pool outside the cap of 5.** Spawns stay inside the definition pool.
- **2 characters.**
  - 2 × 1.75Gi = 3.5Gi, requested and limited (4.5Gi if 2 threads run at once, §4).
  - Beside that, 5 workers × 4Gi = 20Gi at the limit (1Gi requested each).
  - 23.5Gi of limits against 31GB of session nodes, or 15GB with one node out. Guaranteed characters are evicted
    last.
- **One subscription.** Characters share the OAuth account (ADR-0004). The headroom for concurrent streams is
  unmeasured, which is an open risk.
- **Credentials.** ADR-0086 §6 applies. A character outlives the 12h token cap, so the provisioner replaces the pod
  through §3's drain before the cap, under the vault lock. The next `vault-label` is written on the Job before the mint.
- **`limits.turnsPerDay`**, per character across its threads, per calendar day, Europe/Paris.
  - Past it, each thread's head is held unless it is T0.
  - Core files a card whose effect verb `raise_budget` sets a core-side override with a 24h TTL.
- **An alert to the owner** when `turns_last_hour × 24 ≥ turnsPerDay / 2`.

### 10. Later

- A `runtime` field and a Hermes or OpenClaw spike behind the sidecar.
- Multi-character scenes.
- 2 running threads per pod, once the worker is sized at 2Gi (§4).
- GitHub scope per session class.
- gVisor.
- The agent-sandbox spike for the workload. The Job stays until it decides.

## Alternatives considered

- **Threads of mixed trust in one pod, or one pod per tier.** Five rounds of blockers. Threads stay, now that every
  thread is T0/T1.
- **A scheduled SDK rotation** (daily at 04:00). Dropped for owner-opened threads with auto-compaction.
- **Untrusted characters (guest hosts).** Their context carries guest text for good. Dropped: guests never reach the
  fleet.
- **A per-turn tier inside one character.** It recreates the lethal trifecta through the context. Dropped.
- **An ops character that takes alerts.** Replaced by ADR-0086's responders, with the character reading typed
  outcomes.
- **No Bash in characters.** Needless once every character is trusted, and "very, very limiting" (owner).

## Consequences

- A character is a trusted worker that lives on: one session, one pod, threads with the worker's cursor each, and
  Bash within its ceiling.
- New surface:
  - lifecycle `standing`, the `standing:` principal, `Start` and Stop;
  - the re-warm, crash entries, threads and `thread_key`;
  - `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`, `ListResponderCards`, `Speak` and
    `PostToCharacter`;
  - the read ServiceAccount;
  - the memory tool.

## The bet

That with guests outside the fleet and alerts in fresh runs, a long-lived character only ever sees trusted input, so
it can be as capable as the owner's own sessions.

## Not doing

- Untrusted characters.
- Threads of different trust.
- Per-turn tiers.
- A scheduled SDK rotation.
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
4. **Threads and delivery per thread.**
   - `thread_key` on standing transcript entries; one SDK session id per thread.
   - A thread opens on the owner's first message, closes on his close or after 7 days idle, and its transcript stays.
   - At most 1 thread runs at once; the others wait at their own heads.
   - An entry appended while the pod is down is delivered after the re-warm, in seq order within its thread.
   - Answers and Stop pass during a turn.
   - The turn deadline aborts a turn.
   - The idempotency key includes `thread_key`, and an entry whose effect ran is not replayed after a crash.
   - A poison entry is marked done in its thread after three crashes.
5. **Sweeps, re-warm and re-pin.**
   - The idle and stop-grace sweeps skip standing rows. A startup stall alerts.
   - Every terminal path sets `restart_after`.
   - A crash entry is appended to each thread that had a turn running, on every `CRASHED` path, and none for
     Evicted or Preempted.
   - Three crashes stop the re-warm.
   - Re-pin in place drains every thread, and is refused on a widening.
6. **Counting.**
   - Standing rows count only in `MAX_STANDING_SESSIONS`.
7. **Set enforcement.**
   - An untrusted event never reaches a character.
   - Sidecar-local B and C tools are refused outside the set.
8. **Reach.**
   - `SpawnWorker` only for `spawns.allow`; the run's end appends an entry.
   - Spawns count in `MaxDefinition`, `MaxLive` and the character's sub-pool (`limits.spawns.concurrent`).
   - The config check: `MAX_DEFINITION_SESSIONS` is not below the sum of the characters' sub-pools.
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
    - The budget is per character and holds each thread only at its head (moved here from slice 4, which has no budget
      to hold).
    - `raise_budget` sets a 24h override.
    - The alert formula fires.
