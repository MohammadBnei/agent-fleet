# ADR-0084 — Long-lived agents: standing roles beside the workers, one control plane

- **Status:** Proposed (2026-11-16). Owner decision from an architect-companion interview the same
  day, revised after two doubt cycles (same day). The in-fleet long-lived agent (alternative C) is
  kept, and every amendment it needs is designed below. Open owner decisions are marked **Owner
  decision needed**. Each stage is its own later ADR and PR.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:**
  - [ADR-0002](0002-no-orchestration-framework.md): reopened by its own revisit clause (§13).
  - [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md): one pod per warm,
    torn down on Stop or idle (§7).
  - [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1 (no message, no pod) and §2 (no
    retry): §7.
  - [ADR-0062](0062-defined-agents.md) §5 and §7: the pool and the ceiling (§2, §10).
  - [ADR-0064](0064-definition-pod-profile.md) §4: `definitionMethods` (§4).
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§5: the lifetime cap, the
    token per warm, the token in the worker's env, the egress lock (§9).
  - [ADR-0066](0066-agent-service.md) §3–§4 and [ADR-0070](0070-headless-invoke.md) §1 and §6:
    `Invoke` is clients-only, and the re-warm owner (§4, §7).
  - [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4–§5: one thread per standing
    agent, and alerts onto the bridge (§5).
  - [ADR-0037](0037-thot-is-a-worker-task.md): executor reach (§4).
  - [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7: the 12h cap (§9).
  - [ADR-0082](0082-decision-cards.md) §1–§3: a subscription wakes a standing agent, and what a
    turn may cause (§5, §6).
  - `CLAUDE.md`: "cap 5 = max warm pods" and "TTL 12h lifetime cap +60s = `activeDeadlineSeconds`+60s".
- **Related:** [ADR-0004](0004-subscription-oauth-not-metered-api.md) (subscription, not metered:
  §12), [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core commands, provisioner
  executes), [ADR-0078](0078-core-on-the-bridge.md), [ADR-0083](0083-remote-mcp-for-definitions.md)
  (the shape a game MCP would take). infra-bootstrap ADR-0062:53: the App has issues write, which
  §4's `PostIssue` needs.

## Context

Every agent the fleet runs today is a worker: a session whose pod is warmed for a message, does the
work and goes. That fits work. It does not fit roles that should stand.

**The trigger is the game.** `ukubi-game` wants residents at three scales (repo, village, and a
world resident "like a Claude Code session in the ukubi folder"), the game's MCP both ways, and
CK-style character menus. The owner also wants per-agent shape control, live steering, agents
coordinating agents, control from the game, and the bridge's event-driven direction (ADR-0071,
ADR-0078).

**What a resident costs today (measured 2026-11-16).**

- A resident talk is a full worker pod: 1Gi requested and 4Gi limit, fixed in the provisioner
  (`provisioner/internal/k8s/pod.go:173`, `:792-800`). About 240Mi is used.
- A warm takes 3–4 s.
- It sits inside `MAX_EXTERNAL_SESSIONS=2` (`k8s/core.yaml:184`).
- A definition's `resources` is validated (`core/internal/definitions/load.go:650`) and never
  applied.
- The owner expects a handful of short talks per evening.

**What stands in the way (two doubt cycles, checked against the code).**

- **Reach.** A definition pod reaches only the vault proxy, core `:9090` and DNS
  (`k8s/provisioner/definition-egress.yaml:26-49`). `Invoke` and `WaitForResult` are on
  `AgentService` (`proto/agentfleet/v1/agent.proto:88-99`). `definitionMethods` has no Loki,
  journal or peer method (`core/internal/coreserver/auth.go:159-183`). There is no executor and no
  service-account token.
- **Events.** Alertmanager still posts to `/webhook/alertmanager` (ADR-0071's status line).
  `owns`/`subscribes` only route, never wake (ADR-0082 §1). The dispatcher keys a thread by the
  event subject under invoker `bridge` (`core/internal/dispatch/dispatch.go:665`, `:706`, `:853`).
- **Lifecycle.**
  - The idle sweep (`core/internal/sessions/reconcile.go:326`) and the startup-stall sweep
    (`ListStartupStalledIDs`, `WHERE NOT activity_seen`, `store.go:1375`) both reclaim pods.
  - `BackoffLimit: 0` makes a crash a human's re-warm (`pod.go:192-194`, `:827`).
  - A moved pin refuses every warm.
  - `claude-home/<id>` is per session on purpose: it keeps `projects/-workspace/` apart
    (`provisioner/internal/k8s/session_storage.go:18-24`, `pod.go:553`).
- **Credentials.**
  - The vault API is mint (fixed TTL), list and delete (`provisioner/internal/vault/vault.go:587-640`).
  - The token is in env: the worker's `HTTPS_PROXY` (`pod.go:248`), or the sidecar's
    `FLEET_VAULT_PROXY` (`pod.go:840-847`).
  - Teardown revokes the Job's `fleet.bnei.dev/vault-label`.
  - The Role cannot update secrets or patch jobs (`k8s/provisioner/role.yaml:42-43`, `:70-71`).
  - `pod.go:451` refuses `DeadlineSeconds <= 0`.
- **Injection and runaway.** An always-running agent reads every routed entry. ADR-0082 §3 removed
  taint, `consumedSeq` is a high-water mark (`worker/src/session.ts:2012`), and there is no cost cap
  by design.

## Decision

### 1. Two kinds of agents

- **Workers** are today's sessions: one pod per warm, single-shot or resident. They do the work and
  ask the owner through decisions (ADR-0082). Nothing about them changes.
- **Long-lived agents** hold a standing role: an ops agent that watches alerts, reads logs and
  metrics, opens issues and spawns workers; and the game's world and village residents.

### 2. Identity and ceiling

A long-lived agent is a reviewed definition (ADR-0062) with a persona and a mandate, and a new
lifecycle value, `standing`, in `lifecycle.allow`. The loader enforces:

- `mode.default` is `auto` or `default`. `plan` is refused: a standing agent would sit on a plan
  approval forever.
- `limits.concurrency` is 1: one session per role.
- `limits.timeout` is refused: there is no idle timeout (§7).
- `limits.spawns` (workers per hour, per day, at once) and `limits.turns` (turns per day) are
  required (§11).
- `resources` is required and applied (§10).

**One agent, many personas.** The owner's Hermes LXC on Discord is one agent with many threads that
decides for itself what to save. The world and village residents follow that model: they are
personas of **one** long-lived game agent, not one agent each. Nothing in core needs one agent per
resident. Keeping the personas apart is a prompt convention, not a mechanism: Claude Code's
auto-memory is one directory per cwd, not per persona (§8, open questions). The ops agent is a
separate long-lived agent, because its powers differ.

### 3. Runtime: Claude Code first, pluggable later

A `runtime` field names what runs in the pod. Stage 1 knows only `claude-code`. Hermes and OpenClaw
come after a spike (§12), behind the same sidecar contract. Core stays the single control plane:
sessions, transcript, decisions, grants, events and traces. No orchestration framework runs in core.

### 4. Reach: everything through core, on the lease

A standing session's lease gets its own table, `standingMethods`: `definitionMethods` plus the
methods below, each also granted per definition through `sidecarTools`, as `PublishEvent` is
(`auth.go:173-176`). The egress lock does not change.

- **`CoreService.SpawnWorker(definition, input, idempotency_key)`** runs `Invoke` in process.
  - **Principal.** An in-process client (ADR-0066 §3, like the dispatcher's `bridge.yaml`), in
    `definitions/clients/<agent>.yaml`.
  - **Two-sided agreement.** The client lists the worker definitions it may start. Each of those
    definitions lists the client in `invokers` (`core/internal/definitions/spec.go:47`) and allows
    `single-shot` in `lifecycle.allow` (ADR-0070 §1).
  - **Results.** `CoreService.WaitForResult(run_id)` is new, lease-bound, and answers only runs that
    the caller's own client owns. It reuses `agentsvc`'s implementation.
  - **Depth.** Migration: `sessions.spawned_by` (the standing session's id). `SpawnWorker` refuses
    any lease whose session has `spawned_by` set, so the depth is 1. A spawned worker's
    `PublishEvent` keeps ADR-0071 §2's hop limit of 3, counted from its causation.
- **Logs: `ViewLogs`** already exists (`auth.go:142`, component-scoped) and joins the table.
- **Metrics: `CoreService.QueryMetrics`** is a read-only PromQL instant or range query over
  `core/internal/promclient` (`Query`, `QueryRange`, `client.go:54-60`).
- **Issues: `CoreService.OpenIssue`.** Core hands it to a provisioner `PostIssue`, the same shape
  as `SubmitReview` → `PostReview` (ADR-0082 §9). Repos are limited to the definition's
  `owns.repos`.
- **Cluster reads. Owner decision needed** (exclusive):
  - (a) No cluster reads in stage 1. The agent uses alerts, Loki and Prometheus, and spawns a
    `cluster_access` worker (ADR-0037) when it needs `kubectl`.
  - (b) `CoreService.ClusterRead`: core relays fixed read verbs to the executor under a read-only
    scope. This gives core an executor credential.
  - (c) The egress lock admits the executor, and the sidecar holds a read-only executor token.
  - *Recommended:* (a).

### 5. Events: one thread, alerts on the bridge, and subscriptions wake

- **Prerequisite (stage 1):** Alertmanager posts to the bridge, not to `/webhook/alertmanager`.
  This finishes ADR-0071's outstanding item. Without it, no alert reaches a standing agent.
- **One thread.** `Start` (§7) creates the session with invoker `standing` (a reserved in-process
  principal, refused in any client file), `definition_name` = the agent, and `thread_key` =
  `standing`. The existing unique index on `(invoker, definition_name, thread_key)` (migration
  000015) makes it the only one. The admin who started it is recorded as the owner, for the console.
- **Dispatcher change.** When `route` matches a definition whose effective lifecycle is `standing`,
  the dispatcher does not `Open` a thread per subject (`dispatch.go:706`). Instead it:
  - looks up `(standing, <definition>, standing)`, not archived;
  - appends the event entry to that session (core authors it, ADR-0071 §3) with the subject kept in
    its payload;
  - lets the agent read it on `WaitForMessages`. A non-live session gets the entry anyway, and §7's
    re-warm reads it.
  - With no started session, the event becomes a proposal, as today: no Start, no pod.
- **Amends ADR-0082 §1:** for a `standing` definition, a `subscribes` or `owns` match is a wake.
  The worker's `takeIn` filter (`session.ts:2010`, today `human`/`session` only) admits the routed
  event entry as turn-starting for a standing session. The wake carries the event's trust, and §6
  decides what that turn may cause.

### 6. Provenance: a turn acts only on a T0 or T1 cause

Amends ADR-0082 §1–§3. Reading stays free. Causing an effect takes a trusted cause.

- **Core computes the cause, from its own transcript, at the call.** It is the *least* trusted of
  every turn-starting entry delivered above the previous result's `consumedSeq` up to the call.
  That covers the entry that started the turn and every entry merged in mid-turn. The pod sends
  nothing core trusts for this, so a forged sequence number buys nothing.
- **T0** is the owner (console, island, a T0 label or @mention). **T1** is a fleet session's
  `PublishEvent`, trusted when its cause was (ADR-0082 §3). Everything else is untrusted: a routed
  alert, a T2 or T3 text, a core-authored entry (§7's resume and roll messages).
- **`SpawnWorker`, `OpenIssue` and `PublishEvent` need a T0 or T1 cause**, else `PermissionDenied`
  keyed `agent.cause_untrusted`. **`DraftCard` always works.** In an untrusted turn the agent drafts
  a card whose option is the spawn or the issue, and the owner's answer is T0 (ADR-0082 §6).
- **Accepted residual risk.** A T0 turn still carries what the context already holds: earlier
  untrusted entries, compaction summaries, memory files the agent wrote while reading untrusted
  text. The rule bounds what *starts* an effect, not what *shapes* it. The card, and the owner
  reading it, is the bound on shape.

### 7. Lifecycle: started once, kept up by core

Amends ADR-0029 and ADR-0048 §1–§2 for `standing` sessions only.

- **Start is explicit.** `AgentService.Start(definition)` (admin only) creates the session (§5) and
  appends a T0 start message, warm-then-append. A sync never starts one: ADR-0048 §1's human gate
  moves from every warm to the first. `Stop` is the human's off: the pod goes and the row stays.
- **Sweeps.** `ListIdleSessionIDs` and `ListStartupStalledIDs` skip `standing` sessions. Each
  re-warm also appends a resume entry (below), so the stall sweep would not fire anyway; the
  exemption covers a pod that crashes before its first turn.
- **One re-warm owner.** The loop's existing `RewarmFunc` (`reconcile.go:88`, ADR-0070 §6's
  exit-window re-warm, which is `WarmIfIdle`) also takes `standing` sessions. A second selector
  beside `ListRewarmRuns` (`store.go:498`) picks each `standing` session that is not stopped or
  archived, whose phase is CRASHED or TERMINATED, once `now() >= restart_after`. `restart_after` is a new
  column the reconcile sets to terminal time + 5 min when it writes the terminal phase. The same
  migration adds `standing` to `sessions.lifecycle`'s CHECK (migration 000015). After the
  warm, core appends a core-authored resume entry (untrusted, §6). The streaming generator then
  has input, which ADR-0048 §1 says it needs. This is a timestamp, not a counter, so ADR-0048 §2's
  "no retry counter" stands. A crash loop shows on the pod-phase metrics.
- **A Job, not a Deployment.** It keeps the one pod-creation path, its golden tests, its revoke
  paths and `BackoffLimit: 0` (`pod.go:827`). A `standing` Job has no `activeDeadlineSeconds`.
  `pod.go:451` accepts `DeadlineSeconds == 0` only with the `standing` flag. The worker runs
  `lifecycle: resident`, which never exits (ADR-0070 slice 7).
- **Replace on a rollover or a pin change.** Core replaces the session in two cases: weekly
  (Postgres rows stay bounded), and when its `definition_sha` differs from the head (as `Invoke`
  replaces a moved thread, `core/internal/agentsvc/invoke.go:77-83`). The replacement runs at a
  turn boundary, in order:
  1. The turn boundary is: no turn in flight, and no open decision of any kind (question,
     permission, plan, card). A parked decision holds the replacement until it is answered or
     expires (`limits.answerTimeout`; a card's `expires_at`).
  2. Exception: a pin change with no live pod and an open decision. Its decisions go `stale`, as
     `EndMovedRun` does (ADR-0070 slice 11).
  3. Core archives the old session, which withdraws any decision still open (ADR-0082 §5).
  4. Core waits until the old Job is gone from k8s, so two pods never write the memory directory at
     once.
  5. Core creates the successor (same invoker, definition and thread key) and appends a
     core-authored, untrusted "continue" message. It is never T0.
  - What carries over is the memory directory (§8), and nothing else: not the transcript, the SDK
    resume state or the genome.

### 8. Memory: one directory per agent, mounted alone

- **Today.** `CLAUDE_CONFIG_DIR=/claude-home` is `claude-home/<session>` on the shared RWX PVC
  (`pod.go:553`, `k8s/provisioner/pvc.yaml`), and it holds the SDK resume state. The genome
  (`<session>-genome`) and the skills overlay are keyed by session too. All of that stays per
  session.
- **Added.** Only the memory directory is per agent: `claude-home/agent-<name>-memory` is mounted
  over `/claude-home/projects/-workspace/memory`. That path is Claude Code's auto-memory for cwd
  `/workspace` (`session_storage.go:18-24`). Resume state never crosses a roll, and memory does.
- **Storage.** The shared PVC is on the default class, Longhorn, so the memory is replicated, which
  is what the owner asked for. **Recorded risk:** Prometheus left Longhorn after a replica on `.165`
  fed it corrupt reads three times (infra-bootstrap #414,
  `gitops/platform/values/prometheus/values.yaml:190-193`). Control-plane nodes are under Longhorn
  memory pressure (infra-bootstrap ADR-0045:152). The files are few and small. A separate
  per-agent volume would use the same replica set and add nothing.

### 9. Credentials: a rotated file, revocable by label

Amends ADR-0065 §3–§5 and ADR-0075 §7 for `standing` pods.

- **The token leaves env.** A `standing` pod uses the sidecar's egress router (ADR-0075 §7), which
  reads its proxy URL from Secret `fleet-vault-<session>`. The Secret is mounted as a whole volume,
  not a subPath, so kubelet updates it in place, and the router re-reads it on each new tunnel.
  Every host goes to the vault, so `unmatched_host_policy=deny` and the egress lock still decide.
  The worker's `HTTPS_PROXY` is the router, with no token in it.
- **Rotation on the provisioner's own tick**, as it refreshes owner vaults today (ADR-0075).
  - **Expiry** comes from the label, not the vault. The label is `<session>.<unix-ms>` at mint
    (ADR-0065 §3), so a token expires at mint time + 24h (TTL).
  - **When.** A listed `standing` Job whose current label has under 12h left gets a new token.
  - **Order.** Mint the new token. Update the Secret. Patch the Job's annotations: `vault-label` =
    new, `vault-label-prev` = old. After a grace of 15 min, revoke the old label and clear `-prev`.
  - **Teardown** revokes both `vault-label` and `vault-label-prev`.
- **RBAC.** `role.yaml` gains `update` on secrets and `patch` on jobs. The Secret carries an
  `ownerReference` to its Job, so k8s deletes it with the Job, and no sweep is needed.
- **Tunnels.** The 15-min grace outlives a normal model stream. A stream still open on the old token
  at revoke fails, and Claude Code retries the request (accepted). The vault's ~2 min `IdleTimeout`
  closes idle keep-alive tunnels.
- **Expiry bound.** A token dies at most 24h after its last rotation. A stopped provisioner, or a
  Job that is gone (Stop, archive, crash), stops the rotation, and ADR-0065 §3's revoke paths or the
  TTL end the token.
- **Not a lease heartbeat.** A heartbeat flows from the pod to core and carries liveness that core
  trusts. This flows from the provisioner to the vault and a Secret. It is triggered by a Job that
  k8s lists, carries no liveness, and writes no row. Liveness still comes only from core's
  reconcile (ADR-0048 §2).
- **Revoke on widen** (`vault.go:430-470`) already deletes the definition's live Jobs (ADR-0065 §2),
  and §7's re-warm brings the agent back on a fresh token.

### 10. Capacity

Today: `MAX_IN_FLIGHT_TASKS=5`, definitions 3, external 2 (`k8s/core.yaml:170-185`).
`ReserveSlot` counts every live session against `MaxLive` first (`store.go:820-828`). As written, a
standing session would count against `MaxLive` **and** `MAX_DEFINITION_SESSIONS`.

- **Prerequisite (stage 1, first PR):** apply a definition's `resources` to the worker container
  (`load.go:650` validates it; `pod.go` sets 1Gi/4Gi). `TestCreateWorkerPod_ResourcesWithinLimitRange`
  covers both.
- **Size.** Request 1Gi, limit 2Gi. Idle use is about 240Mi, but a long resident context grows the
  CLI's heap, and an OOM is a 5-min outage (§7). Two agents hold at most 4Gi at limit, beside
  workers that already oversubscribe worker-02's 16GB (infra-bootstrap
  `docs/infrastructure-actual.md:254`).
- **Owner decision needed: the standing pool** (exclusive):
  - (a) `MAX_STANDING_SESSIONS=2`, outside both pools. `ReserveSlot`'s live count and the
    definition-pool count add `AND lifecycle <> 'standing'`, and a standing session checks only its
    own pool. Workers keep all 5 slots.
  - (b) `MAX_IN_FLIGHT_TASKS` 5 → 7, with the standing pool inside it and inside the definition
    pool, raised 3 → 5.
  - (c) Carve 2 out of today's 5. Workers drop to 3.
  - *Recommended:* (a). `CLAUDE.md`'s "cap 5 = max warm pods" becomes "5 worker pods, plus the
    standing pool".
- **Owner decision needed: the pool spawned runs draw from** (exclusive). They are client-owned, so
  today they land in the external pool of 2 that the game also uses.
  - (a) The external pool, as today. A busy agent can starve the game.
  - (b) The definition pool, outside external. Spawns can take admin definition slots, never the
    game's.
  - (c) A per-agent sub-pool, `limits.spawns.concurrent` (default 1), inside the definition pool.
  - *Recommended:* (c). It bounds each agent, and starves neither the game nor the workers' 5.

### 11. Runaway: limits that bound loops

- **Spawn rate:** `limits.spawns` per hour and per day, refused `ResourceExhausted` keyed
  `agent.spawn_limit`.
- **Depth:** 1 for spawns (§4). PublishEvent keeps ADR-0071's 3 hops and its 3 agent wakes per
  thread per 24h, applied to the `standing` thread.
- **Owner decision needed: the turn budget vs "no cost cap by design"** (`CLAUDE.md`, ADR-0004).
  Exclusive:
  - (a) A budget: `limits.turns` per day. Past it, core makes no new entry turn-starting and drafts
    one card asking to raise it.
  - (b) No budget.
  - *Recommended:* (a). It counts turns, not money: the lock was about metered spend, and a loop is
    a different failure.
- **Owner decision needed, separately (yes or no):** an alert on turns per hour at half the budget,
  or at a fixed rate under (b). *Recommended:* yes.

### 12. Other runtimes: what the spike must answer first

The worker contract is about 8k lines of TypeScript (`worker/src/*.ts`): the fleet gate in
`canUseTool`, the `{nb: 1}` stamp (`session.ts:698`), transcript shaping, and resume through
`RESUME_SESSION_ID` and `SaveAgentSessionId`. Hermes or OpenClaw must reimplement it or sit under
it. The spike answers, in order:

1. **Auth.** Subscription OAuth is Claude Code's. A third-party harness likely needs a metered key,
   which conflicts with ADR-0004. If so, the spike stops and the owner decides.
2. **The gate.** Does every tool call reach a fleet-owned `canUseTool`, and can a pending answer
   park with no pod?
3. **Sub-agents and schedulers.** Both must route through core (`SpawnWorker`, `Start`), or they
   bypass the control plane (alternative D).

Core and worker changes are required either way.

### 13. The game and ADR-0002

- **The fleet has no voice runtime.** A no-pod runtime fleet-side could only live in core, which is
  alternative B.
- **Game-side, by owner direction (2026-11-16): start simple.**
  - **Repo residents** are a direct LLM call on the game's voice path (`ukubi-game`
    `internal/voice`). Today it is one turn on a small model: 160 max tokens, 10 s, an in-memory
    daily budget, a metered key, no history (`anthropic.go:17-23`, `budget.go:8`).
  - The game extends that path with history per thread, persona and facts.
  - No escalation in v1: only the player's "Order an errand" verb starts a worker.
  - **Town, village and world talks** go to Hermes or a fleet session directly, behind one seam in
    `ukubi-game` `fleet/`.
- **Accepted in v1: two brains for a repo resident.** Its voice talk knows only the facts the game
  passes about its fleet work.
- **Embodiment** comes later, through the game's MCP. ADR-0083's remote-MCP shape is the related
  one.
- **ADR-0002 is reopened narrowly,** by its own revisit clause. It covers runtimes inside the fleet,
  behind the sidecar and under core. A framework as a coordinator over the fleet stays rejected.

### 14. Stages, in order

Each is its own later ADR and PR.

1. **The standing role on Claude Code**, with prerequisites first: `resources` applied, and
   Alertmanager on the bridge. Then:
   - the `standing` lifecycle and ceiling (§2);
   - `standingMethods`, `SpawnWorker`, `WaitForResult`, `QueryMetrics` and `OpenIssue` (§4);
   - the one thread and the dispatcher change (§5);
   - provenance (§6);
   - Start, the sweep exemptions, the re-warm and replace (§7);
   - the memory mount (§8);
   - the rotated token (§9);
   - the pools and runaway limits (§10–§11).
2. **The `runtime` field** and the Hermes/OpenClaw spike (§12).
3. **Live steering** through core: pause, redirect, goals, budget.

## Alternatives considered

- **A. Boring: resident definitions on Claude Code only, with rotation.** Rejected as the whole
  answer: an idle pod costs a worker's envelope, and a re-warmed resident reacts slowly. It is
  partly kept as stage 1.
- **B. An agent loop inside core, no pod.** Rejected: it puts an agent inside core's trust boundary
  on a single replica, and it is how a framework creeps in. It also rules out a fleet voice runtime.
- **C. Two kinds, pluggable runtime behind the sidecar, core the only control plane.** Chosen.
- **D. Hermes as an orchestrator outside the fleet, calling `AgentService`.** Rejected: two control
  planes. The game may still reach Hermes behind its own seam (§13). That is the game's choice, not
  a second fleet control plane.
- **A Deployment** (k8s restarts without core), **extending the token** (the vault has no extend),
  **a per-agent claude-home** (shares resume state across rolls), **a separate per-agent Longhorn
  PVC**, and **a scheduled ~11h rotation** (owner): each rejected above.

## Consequences

- A standing agent holds a pod all the time, in its own pool (§10).
- A token dies within 24h of its last rotation, instead of at 12h.
- ADR-0048 §1's gate is per Start, not per warm, for `standing` sessions.
- An alert can make the ops agent think, never act. An act from an untrusted cause is a card.
- `CLAUDE.md` and `DECISIONS.md` keep "no orchestration framework (Hermes/OpenClaw rejected)"
  until stage 2 is accepted.

## The bet

One control plane can host always-on agents. Exempting them from the 12h cap, with the provisioner
rotating the vault token into a file, keeps both rules that matter: no secret in the worker, and
core commands while the provisioner executes.

## Not doing, open questions

- **Not doing:** a framework over the fleet; an agent loop in core; a fleet voice runtime; a
  heartbeat; a Deployment.
- **Owner decisions needed:** cluster reads (§4); the standing pool and the spawn pool (§10); the
  turn budget and the alert (§11).
- **Open:** Hermes or OpenClaw (§12); which grants an ops agent may hold beyond §4; the game MCP
  contract; the rollout order across the game's three scales.
- **Open: multi-character talk.** This covers group scenes, residents talking to each other,
  keeping characters apart, and routing who speaks. Starting direction:
  - one scene is one thread;
  - each output entry carries a `speaker` tag (persona id) that the game maps to a face and a
    bubble;
  - one director turn chooses the next speaker;
  - each persona keeps a file of its own inside the agent's memory directory, by convention.
  Real isolation would need a session per persona, which is the cost §2 avoids.
