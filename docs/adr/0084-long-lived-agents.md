# ADR-0084 — Long-lived characters: one trusted pod per character, kept up by core

- **Status:** Accepted (owner, 2026-11-16)
  - The owner chose one pod per character and adopted [ADR-0086](0086-rule-of-two.md) (Rule of Two).
  - Guests never reach the fleet. Every character is the owner's, and so trusted.
  - Alerts go to ADR-0086's single-shot first responders.
  - Choices marked **default, owner may change** are the coordinator's defaults.
  - **Owner to confirm** marks a consequence the owner has not yet seen.
  - Mechanisms live in the build-slice appendix.
- **Date:** 2026-11-16
- **Amended 2026-11-17:** the character API for the game (§7, [ADR-0086](0086-rule-of-two.md) §4, [ADR-0082](0082-decision-cards.md) §7); slices 14 to 19.
- **Built:** appendix slice 3: the `standing` lifecycle, the `standing:<name>` principal, and an admin's Start and Stop
  (`DashboardService.StartStanding`, `StopStanding`). Start makes the row and provisions nothing; the first message
  warms it, until slice 5's re-warm.
- **Built:** appendix slice 10: token cap (migration 000035: `sessions.pod_started_at`, stamped by `SetPodImages`).
  - `repinStanding` replaces a live pod older than `PodLifetime - ReplaceMargin` (12h - 30m) through the re-pin
    drain, at the same version; it also runs when the registry cannot be read. The migration backfills running
    characters as aged, so each is replaced once instead of dying undrained. A test pins `PodLifetime` to the
    provisioner's constant. The vault label before the mint, and revocation at once, are the existing paths.
- **Built:** appendix slice 8: reach (migration 000034: `sessions.spawn_reported_at`).
  - New spec: `spawns.allow` (a standing definition only; each entry a valid single-shot definition that is not a
    first responder), `limits.spawns` (`concurrent` default 1, `perHour` 6, `perDay` 24, applied where read) and
    `cluster.logNamespaces`. A widening of `spawns.allow` or `logNamespaces` refuses a re-pin in place. Five sidecar
    tools join the ceiling and only a standing definition may hold them: `spawn_worker`, `wait_for_result`,
    `open_issue`, `list_responder_cards`, `view_logs`.
  - `CoreService.SpawnWorker` is an in-process Invoke under the principal `standing:<name>`, T1 (set BC), refused
    off `spawns.allow`. It counts in `MaxLive`, the definition pool and the sub-pool (`sessions.ReserveSlot`: live
    runs with that invoker, against the character's pinned `limits.spawns.concurrent`), and not in `MaxExternal`.
    The hourly and daily caps count starts, and meet a new run only: a retry of a key at the cap returns its thread.
    `WaitForSpawn` answers only the character's own runs. Core's sweep (`agentsvc.RunSpawnReports`) appends one
    turn-starting entry to the character's main thread when a run ends (done, failed, expired), keyed
    `spawn-end:<run>`; a stopped character takes none and the run is marked anyway.
  - The pool-sum check: at startup, after a first good definitions sync, core refuses a `MAX_DEFINITION_SESSIONS`
    below the sum of the characters' sub-pools (`cmd/core/spawnpool.go`), and logs it on every later sync.
  - `OpenIssue` is limited to `owns.repos` and relayed to the provisioner's `CreateIssue` (the App needs the Issues
    write permission). `ViewLogs` for a definition session needs the grant and a namespace on
    `cluster.logNamespaces`; a repo session and the provisioner are unchanged. `ListResponderCards` reads the
    `alert_group` cards and the remediations (kind, state, core's card text, action, target, responder): never the
    agent claim, labels, annotations or alert text.
  - Not in this slice: `QueryMetrics` (§6).
- **Built:** appendix slice 13: budget and alert (migration 000036: `sessions.budget_override_until`).
  - `limits.turnsPerDay` (standing only, 1-10000, default 200 applied where read). A turn is a worker result entry
    (core's crash results are not), counted per character across its threads since midnight Europe/Paris.
  - The hold is in `StreamHumanMessages`: with the day's turns spent and no override standing, a thread's head that is
    T1 (from a session) waits, and every turn-starting entry behind it on that thread waits in order, never skipped. The
    owner's message passes a thread with nothing held; answers, permission responses and aborts always pass.
  - `agentsvc.RunBudgets` (every minute) publishes `agentfleet_character_turns_last_hour` and
    `agentfleet_character_turns_per_day`, and files one core card a day per spent character (subject kind `character`,
    its definition name). Its `raise_budget` option, a core verb bound to the writer's own name, sets the 24h override.
  - The alert rule (`turns_last_hour * 24 >= turnsPerDay / 2`) is a draft PR on infra-bootstrap.
- **Built:** appendix slices 14 and 15: the persona block and the character list (no migration). A started character whose head is invalid or gone is still listed from its pinned version; WatchCharacters names a character that left the set in `removed`. `building:` homes take the atlas id alphabet (`kind:name`).
  - `persona` in the spec (standing only), checked at load and on every sync (`definitions.validatePersona`): bounds
    and `home` syntax as in A2. It rides the effective spec, so a persona edit is a new sha and the §3 re-pin drains.
  - `AgentService.ListCharacters` and `WatchCharacters` (`agentsvc/characters.go`): every valid standing head, started
    or not, in the authz table (`{member: true}`) and `surfaceCalls`, refused unless `AnswersCards()`. Watch polls every
    2 s and sends changed characters whole, after a baseline of all.
  - `state` is read from the row and the transcript (`sessions.CharacterRuntime`): `running` keeps a drain's reason in
    `why`; `held` is a drain (pin drain, token refresh), a crash back-off with no pod, or the budget; `stopping` is a
    stopped row whose pod is still live ("pod drain"). `threads` lists every thread, open ones first (main first), then parked ones marked `parked`; `spawns.used` is
    the spawned runs holding a pod. `open_cards` counts open cards on the character's session and its spawned runs
    (by invoker `standing:<name>`), so it needs no A6 join yet.
- **Built:** appendix slices 16 to 19: the lifecycle, thread reads, cards joined to a character, speech and the kind-local
  additions (no migration).
  - `AgentService.StartCharacter`, `StopCharacter`, `ReadCharacterThread`, `WatchCharacterThread` and
    `CloseCharacterThread` (`agentsvc/lifecycle.go`), each in the authz table (`{member: true}`) and `surfaceCalls`, each
    refused unless `AnswersCards()`. Start, Stop and Close are `DashboardService.StartStanding`, `StopStanding` and
    `CloseStandingThread` (the console's own, `dashboard/standing.go`): `agentsvc` only turns an outcome that says the
    state already is what was asked (`ErrStandingStarted`, `ErrStandingStopping`, `ErrStandingStopped`,
    `ErrStandingNever`, `ErrThreadParked`) into the character's state, and the not-found outcomes into
    `agent.character_not_found` and `agent.thread_not_found`. Start and Stop answer a `Character`, as `ListCharacters`
    shows it.
  - Stop keeps every thread's SDK session id, cursor and waiting entry. It settles only a turn in flight: each running
    thread's head entry gets a core `aborted` result (`from` fleet, so it counts no turn), which moves the cursor.
    `RestartStanding` no longer drops session ids or moves cursors; it restarts the crash counters.
  - Closing parks: `CloseThread` keeps `agent_session_id`, and an owner message to the key reopens it from that entry
    with the id (`transcript.noteThread`). `GetSession` returns parked threads' ids. The worker resumes a thread it ended
    in this pod from the id it had (`endedKeys`), else from the boot-time id. The 7-day idle close is the same call.
  - A resumed stream can start below the abort that stopped the earlier pod, so `StreamHumanMessages` skips a main abort
    older than the row's `pod_started_at`.
  - Thread reads filter the one transcript sequence by thread and return the console's entries, tool results included,
    from a parked thread too; the watch polls once a second and ends at 90 s.
  - `Decision.character` and `thread_key` come from the session row (`decisions.Record`): the invoker `standing:<name>`
    covers the character's session and the runs it spawned, and a `character` card subject names it otherwise.
    `CardSubject.name` is the new field; `normalize` writes it only and reads `repo` of an old card.
  - `WatchSpeechResponse.thread_key` is the turn's thread; `WatchSpeech` sends one empty frame first.
  - `local/kind/30-core.yaml` sets `FLEET_ADMIN_ORIGINS` to `http://127.0.0.1:8152`; `local/kind/definitions/dev-speaker.yaml`
    is the Speak-granted standing character. `TestDevSpeakerIsKindLocalOnly` and `TestManifestsLoad` pin both.
- **Built:** appendix slice 12: the game channel (no migration).
  - `AgentService.PostToCharacter(character, text, thread_key)`: an admin on the console or an admin origin (the
    surface list gains it), an owner (T0) message appended as the console's `PostMessage` does, warming a character
    whose pod is down. A stopped or unknown character takes nothing; an island or a client is refused.
  - `CoreService.Speak(text, face)` (tag C, sidecar tool `speak`, a standing character's tool granted through
    `sidecarTools`). Core hands the line to `AgentService.WatchSpeech`, a stream for the same admin principals
    (`agentsvc/speech.go`). It is never stored, never on the bridge, wakes nothing, and a line spoken with nobody
    watching is dropped (the tool says `delivered: false`). At most 20 lines per turn, counted from the turn the
    effect key names; a replayed key is one line. Text is at most 1000 characters, a face `[a-z0-9_-]{0,32}`.
  - The game server holds no connection to a character; the game's browser calls both RPCs under core's own session.
- **Built:** appendix slice 7: set enforcement. The dispatcher appends an event to a started character
  (`Dispatcher.characters`) only when it is T1: a trusted `PublishEvent` hand-off naming the character, from a session
  whose stored set lacks A (looked up through `published_events`). Alerts, GitHub events, unaddressed, unverified or
  tainted publishes never reach it, and a stopped character takes nothing. The sidecar-local B and C tools already ask
  core first (every sidecar tool is a tagged CoreService call, ADR-0086 §2); no new local tool exists to gate yet.
  The appended entry carries no causation, so the character's own publishes about it are unvouched.
- **Built:** appendix slice 6: counting. Standing rows count only in `MAX_STANDING_SESSIONS` (default 2) and leave
  `MaxLive`, `MaxDefinition` and a definition's concurrency. The spawn sub-pool and the pool-sum
  config check needed `limits.spawns` and were built in slice 8.
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
  - Not in this slice: `spawns.allow` in the widening test (the spec has none yet).
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
  - (Amended 2026-11-17, built in slice 16: Start resumes each thread's kept SDK session id and cursor, A4.)

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
  - (Amended 2026-11-17, built in slice 16: closing parks the thread and keeps its SDK session id, A4.)
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
  - `pods/log` comes only through Roles in a named namespace list: `agent-fleet`, `ukubi-game`, `ukubi-realm`,
    `ukubi-meet` and `monitoring`.
  - Never `secrets`, `configmaps`, RBAC objects, `*/exec`, `*/proxy` or `nodes/proxy`.
  - The egress lock admits the API server for granted pods.
  - **The one reader is the sidecar tool `kubectl_read`** (A5). It is served only when the pod holds the grant and
    the session's stored set holds B (the provisioner passes `FLEET_CLUSTER_READ`, `FLEET_CLUSTER_LOG_NAMESPACES` and
    the set; a missing set serves nothing). Verbs: `get`, `describe`, `logs`, `events`, `top`, `api-resources`.
    Flags are an allowlist (no `--as`, `--server`, `--token`, `-f`, `--raw`, `-w`), and `logs` must name one of the
    definition's `logNamespaces` with `-n`. It runs kubectl with an environment built for it, as the mounted token.
    The check is in the sidecar and the grant in core; the ADR-0086 "asks core first" call is not made for it yet.

### 7. The game, for the owner only

- **Owner talk from the game goes from the browser straight to core**, under core's own OIDC session, from an admin
  origin (`FLEET_ADMIN_ORIGINS`). That path calls `AgentService.PostToCharacter(character, text)`, as T0.
- **The game server has no connection to characters.** Guests use the game's voice path.
- **`CoreService.Speak(text, face)`**, granted per definition and tagged C, sends a character's line to the owner's
  browser on that admin origin. It never goes on the bridge and never wakes anything.
  - Core bounds it at 20 lines per turn.
  - A character that does not appear in the game has no `Speak` grant.

(Amended 2026-11-17: see A1 to A8 for the game's character API.)

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

## Amendment 2026-11-17: the character API for the game

Owner-agreed (via the game session, 2026-11-17). The game draws characters, so it needs to list them, start and stop
them, read their threads, and join cards to them. This amends §7 (the game surface), [ADR-0086](0086-rule-of-two.md) §4
("steer and stop stay in the console") and [ADR-0082](0082-decision-cards.md) §7 (the surface's call list). The
decisions are final; mechanisms are in appendix slices 14 to 19.

### A1. One surface, owner only

- Every call below is on `AgentService`, owner-only (platform-admins), T0, and callable from the browser on
  `FLEET_ADMIN_ORIGINS` (`https://game.bnei.dev`) under core's own OIDC session.
- Each call is added to **both** the authz table and `surfaceCalls` in `core/internal/agentsvc/authz.go`. Islands and
  `embed.origins` never get them. The game server has no connection to characters (§7 stands).
- "Owner-only" is built from two things, because the table only has member and client flags. Each new row is
  `{member: true}` with `client` false, so the game server's bearer (`FLEET_CLIENT_TOKEN_GAME`, port 9091) cannot call
  it. Each handler refuses unless `AnswersCards()` (admin, console or surface). A handler must not use `Admin()`: it is
  false on a surface, because `Origin` is set, and the game would be denied.
- ADR-0082 §7's "list, get, watch, answer" widens to this list, and only to it.

### A2. The persona block

- A definition gains a reviewed `persona`: `display_name`, `role`, `traits[]`, `relations[{to, kind}]`, `portrait`,
  `home`. It is read-only data from the definition's synced file; changing it is a definition review like any other.
- `home` is opaque to the fleet and checked by syntax only: `world`, `village:[a-z0-9-]+` or `building:<atlas id>`. The
  game resolves it.
- Every other field is bounded, and a violation is refused at load:
  - `display_name` up to 40 characters, `role` up to 80, up to 8 `traits` of up to 40 each.
  - `portrait` is an atlas or asset id (`[a-z0-9-]+`, up to 64), never a URL, so the game never fetches an arbitrary
    host from the admin origin.
  - `relations` has up to 16 entries. `to` is a definition name (the definition-name regex) and `kind` is an enum
    (`friend | mentor | rival | family | colleague`).
- A persona change is a sha change, so it drains and replaces the pod like any definition change (§3 re-pin). Cosmetic
  edits are not exempt: persona is part of the reviewed file, and one rule is cheaper than a second class of change.

### A3. Reading characters

- `ListCharacters` and `WatchCharacters` (a baseline frame, then changes, ending at 90 s like `WatchDecisions`) return,
  per character: `name`, `display_name`, `definition {name, sha}`, `persona`, `state`, `why`, `threads`, the running
  turn, `budget`, `spawns`, `can_speak` and `open_cards`.
  - `state` is `stopped | starting | idle | running | held | stopping`. `why` is a short reason when the state is not
    plain: budget, token refresh, crash back-off, pin drain.
  - `threads` is `[{key, title, running, last_at, parked}]`: open threads first, main first, then parked ones, which the next owner message to the key resumes. `title` is the owner's first line, cut to 60 characters.
  - The running turn is `{thread, started_at}`. `budget` is `{turns_today, limit, held}`. `spawns` is `{used, limit}`.
  - `can_speak` is whether the definition holds the `Speak` grant (§7).
  - `open_cards` is the count of open cards joined to the character (A6).

### A4. Start and stop move; steering stays

- `StartCharacter` and `StopCharacter` return the new state at once. By owner decision, start and stop for characters
  move to `AgentService`; ADR-0086 §4's "steer and stop stay in the console" now reads: **steering stays in the
  console**. Stopping a character never answers a card.
- **Stop and close are resumable, like Claude Code sessions (owner 2026-11-18).** A simple stop, with no confirm page.
  - `StopCharacter` stops the pod and keeps every thread's SDK session id and cursor. `StartCharacter` resumes each
    thread from them, so a stop then start loses no context and no waiting entry. Only an in-flight turn is aborted,
    like Ctrl-C, and appends an abort entry.
  - `CloseCharacterThread` parks the thread: its SDK session id and transcript are kept, and a later owner message to
    that thread key resumes it with full context. The 7-day idle close parks the same way. There is no reopen RPC:
    reopen is the owner's next `PostToCharacter` on that key.
  - Both act in place from a surface, as `StartCharacter` does. Being resumable, they stay inside what ADR-0082 §7
    already accepts from the admin origin for a reversible "yes"; the confirm page is not used for them.
- **Boundary states.** `StartCharacter` on a `stopping` character returns state `stopping` with `why` "pod drain",
  not an error, so the game shows it and retries. Starting an already started character and stopping an already
  stopped one return the current state. Declared keys (ADR-0081), listed on each RPC's method option:
  `agent.character_not_found` and `agent.thread_not_found`. Nothing else is a new key; the rest stays `server.internal`.

### A5. Threads the owner can read

- Standing rows are owned by `client:standing:<name>`, so today's thread reads refuse the owner. `ReadCharacterThread(name,
  thread, since)` and `WatchCharacterThread` return the owner-readable transcript of one character thread, in the same
  entry shapes as the console.
- `CloseCharacterThread(name, thread)` is the §4 close, on the owner's word. It parks the thread and is resumable (A4).
- **Full transcript (owner 2026-11-18).** Thread reads from the game return the full transcript, the same entries the
  console shows, tool results included. This is wider than card text and speech: any script on `game.bnei.dev` can
  read it, and it can hold `view_logs` output, issue and PR text, permission inputs and spawn results. The owner
  accepted that on the admin origin; the surface shapes are not narrowed.

### A6. Cards and decisions join to a character

- `CardSubject.kind = character` already exists: slice 13's budget card sends it with the definition name in `repo`, and
  the card path keys it `character/<repo>`. This moves the name to a new `name` field.
  - The thread key `character/<name>` is unchanged. Stored cards keep `repo`; reads accept both `name` and `repo` for
    the `character` kind, and new cards write `name` only. No migration of stored rows.
  - Only core files `character`-subject cards (the budget card); an agent's `draft_card` still cannot carry the kind
    (`decisions/card.go` refuses it unless core drafts). Agent cards join to a character through `Decision.character`.
- `Decision` gains `character` and `thread_key`, set by core from the session row, never by the session. The game
  joins cards and open decisions of every kind to a character without a second lookup.
  - A spawned run (§6: T1, principal `standing:<name>`, its own definition) has a session row whose definition is not
    the character. Core sets `character` from the session's owner principal when it is `standing:<name>`, so spawned
    runs join too.
  - Cards core files itself (budget, alert-group responder, unmerged-close) have no character session row. For these,
    `character` comes from a `character` subject when there is one, and is empty otherwise.

### A7. Speech

- `WatchSpeechResponse` gains `thread_key`; `WatchSpeech` sends one empty first frame so the client knows it is
  connected.
- Speech stays live-only and best effort: no replay, no store, and a line may be lost.

### A8. Trust notes

- Everything here is owner-only and T0 from the admin origin; nothing widens a character's set (ADR-0086 §1 stands).
- Fleet text (persona fields, thread titles, transcript, `why`, card text) is model-authored or definition-authored.
  Consumers must render it as text, never as markup.
- The game's "Ask about" prefill carries only atlas ids or character names, never a player-chosen string.
- Speech is best effort; the game must not depend on it for anything the owner must see (cards are the record).
- An injected script on the game origin can call these, as ADR-0082 §7 already assumes for cards. Start, stop
  and close act in place and are resumable (A4); thread reads return the full transcript (A5). The card
  rules are unchanged.
- A registered game-server client and an island origin are refused on every new RPC (A1).

### A9. Kind-local development only

- `FLEET_ADMIN_ORIGINS` defaults to `http://127.0.0.1:8152` in the kind-local values only, never in `k8s/core.yaml`.
  It lives, and is overridden, in `local/kind/30-core.yaml` (core's env there), and nowhere else.
- That origin must not also be in `FLEET_DEV_ORIGINS`: the member interceptor checks dev origins first and would treat
  the call as the console, so the kind check would never exercise the surface path. A core unit test asserts `Principal.Surface` is true for that origin;
  in kind the observable signal is a call outside `surfaceCalls` being refused. An http admin origin is accepted only with `FLEET_AUTH_DISABLED`, which kind-local sets.
- A `dev-speaker` standing definition holds the `Speak` grant and a persona with `home: world`. It lives outside the
  prod-synced `definitions/` path, and a test proves prod never loads it.
- Kind-local peak memory with a standing character is measured (`docker stats`) and reported in the PR.

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
   - (Amended 2026-11-17: slice 16 changes close to park, keeping the SDK session id.)
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
14. **Persona.**
    - The `persona` spec block, with the `home` syntax check, in the loader and the registry.
    - A bad `home`, a URL `portrait`, an unknown relation `kind` or an over-long field is refused at load. A changed
      persona is a definition change (sha) and drains the pod.
15. **Read characters.**
    - `ListCharacters` and `WatchCharacters` (baseline, changes, 90 s end), with `state`, `why`, threads, the running
      turn, budget, spawns, `can_speak` and `open_cards`.
    - Proto in `AgentService`, Go and TS regenerated, `buf breaking` clean.
16. **Start, stop and thread reads.**
    - `StartCharacter` and `StopCharacter` return the new state at once.
    - `StopCharacter` keeps every thread's SDK session id and cursor, and `StartCharacter` resumes them (today's
      `RestartStanding` clears them: this slice changes it).
    - `ReadCharacterThread`, `WatchCharacterThread` and `CloseCharacterThread`, owner-readable on
      `client:standing:<name>` rows, returning the full transcript (A5). Close parks the thread; a later owner message
      resumes it, and the idle close parks too.
    - Tests: stop then start resumes every thread with its SDK session id and cursor; stop aborts only the in-flight
      turn; close then an owner message resumes the thread with context; Start on a `stopping` character returns
      `stopping`; a thread read returns tool results.
    - The keys in A4 are declared in the proto (ADR-0081).
17. **Cards join.**
    - `CardSubject.kind = character` moves its name from `repo` to `name`; reads accept both; stored cards are not
      migrated.
    - `Decision.character` and `thread_key` set from the session row, empty for core-filed cards without a character
      subject. `open_cards` is the joined count.
18. **Speech.**
    - `thread_key` on `WatchSpeechResponse`, and the empty first frame.
19. **Surface test and kind-local.**
    - Each new RPC with Origin `https://game.bnei.dev` and an admin cookie is allowed, without admin is denied, and each
      is in both the authz table and `surfaceCalls`.
    - On every new RPC, a non-admin member with an empty Origin is denied by the handler (`AnswersCards()`), and an
      admin from an embed origin is denied.
    - Each new RPC is `{member: true}` with client false: a registered client (9091) and an island origin are refused
      on every one.
    - The kind-local `FLEET_ADMIN_ORIGINS` default, the `dev-speaker` definition outside `definitions/`, and the test
      that prod never loads it. Peak memory measured.
