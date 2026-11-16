# ADR-0084 — Long-lived agents: standing roles beside the workers, one control plane

- **Status:** Proposed (2026-11-16). Owner decision from an architect-companion interview the same
  day, revised after a doubt cycle (same day): the in-fleet long-lived agent (alternative C) is kept,
  and every amendment it needs is designed below. Open owner decisions are marked **Owner decision
  needed**. Each stage is its own later ADR and PR.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:** [ADR-0002](0002-no-orchestration-framework.md) (reopened by its own revisit
  clause, §11), [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) (one pod
  per warm, torn down on Stop or idle: §6), [ADR-0048](0048-one-session-one-pod-one-shared-home.md)
  §1 (no message, no pod: §6) and §2 (no retry: §6's re-warm),
  [ADR-0062](0062-defined-agents.md) §5 and §7 (pool and ceiling: §2, §8),
  [ADR-0064](0064-definition-pod-profile.md) §4 (`definitionMethods`: §4),
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§5 (lifetime cap, token per
  warm, token in the worker's env, the egress lock: §7),
  [ADR-0066](0066-agent-service.md) §3–§4 and [ADR-0070](0070-headless-invoke.md) §1 (`Invoke` is
  clients-only: §4), [ADR-0037](0037-thot-is-a-worker-task.md) (executor reach: §4),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7 (the 12h cap: §7),
  [ADR-0082](0082-decision-cards.md) §1–§3 (what a routed entry may cause: §5).
- **Related:** [ADR-0004](0004-subscription-oauth-not-metered-api.md) (subscription, not metered:
  §10), [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core commands, provisioner
  executes), [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4 (`subscribes`, already
  anticipating "an always-on Hermes-type agent"), [ADR-0078](0078-core-on-the-bridge.md),
  [ADR-0083](0083-remote-mcp-for-definitions.md) (the shape a game MCP would take).

## Context

Every agent the fleet runs today is a worker: a session whose pod is warmed for a message, does the
work and goes. That fits work. It does not fit roles that should stand.

**The trigger is the game.** `ukubi-game` wants residents at three scales (repo, village, and a
world resident "like a Claude Code session in the ukubi folder"), the game's MCP both ways (residents
move, make faces), and CK-style character menus. The owner also wants per-agent shape control, live
steering, agents coordinating agents, control from the game, and the event-driven direction of the
bridge (ADR-0071, ADR-0078).

**What a resident costs today (measured 2026-11-16).** A resident talk is a full Claude Code worker
pod: 1Gi requested and 4Gi limit, fixed in the provisioner (`provisioner/internal/k8s/pod.go:173`,
`:792-800`) while about 240Mi is used, 3–4 s to warm, inside `MAX_EXTERNAL_SESSIONS=2`
(`k8s/core.yaml:184`), the pool all member and client sessions share. A definition's `resources`
is validated (`core/internal/definitions/load.go:650`) and never applied. The owner expects a
handful of short talks per evening, mostly status and advice.

**What stands in the way, found by the doubt cycle.**

- *Reach.* A definition pod reaches only the vault proxy, core `:9090` and DNS
  (`k8s/provisioner/definition-egress.yaml:26-49`). `Invoke` is clients-only on `AgentService`
  (`proto/agentfleet/v1/agent.proto:88-94`). `definitionMethods` has no Loki, journal or peer
  method (`core/internal/coreserver/auth.go:159-183`). The executor is out of reach, and
  `automountServiceAccountToken` is false.
- *Lifecycle.* The idle sweep reclaims idle pods (`core/internal/sessions/reconcile.go:326`).
  `BackoffLimit: 0` makes a crashed session a human's decision to warm again
  (`pod.go:192-194`, `:827`), so an eviction, OOM or node loss is never restarted. A session with no
  message has no pod (ADR-0048 §1). One pod per warm, torn down on Stop or idle (ADR-0029).
- *Credentials.* The vault API is mint with a fixed TTL, list and delete; nothing extends a token
  (`provisioner/internal/vault/vault.go:587-640`). A definition pod's token is in the worker's
  `HTTPS_PROXY` (`pod.go:248`); a repo pod's is in the sidecar's `FLEET_VAULT_PROXY` env
  (`pod.go:840-847`). Env is immutable, and the provisioner has no push to a sidecar. A widening
  reconcile revokes the vault's sessions (`vault.go:430-470`). Both pod kinds die at 12h (ADR-0065
  §3, ADR-0075 §7).
- *Injection and runaway.* An always-running agent reads every routed entry: a stranger's issue
  text, alert annotations, log lines. ADR-0082 §3 removed taint, and there is no cost cap by design.

## Decision

### 1. Two kinds of agents

- **Workers** are today's sessions: one pod per warm, single-shot or resident. They do the work and
  ask the owner through decisions (ADR-0082). Nothing about them changes.
- **Long-lived agents** hold a standing role: an ops agent that watches alerts, reads logs and
  metrics, opens issues and spawns workers on a subject; the game's world and village residents.

### 2. Identity and ceiling

A long-lived agent is a reviewed definition (ADR-0062) with a persona and a mandate, and a new
lifecycle value, `standing`, in `lifecycle.allow`. The loader holds it to this ceiling:

- `mode.default` is `auto` or `default`; `plan` is refused, because a standing agent would sit on a
  plan approval forever.
- `limits.concurrency` is 1: one session per role.
- `limits.timeout` is refused: there is no idle timeout (§6).
- New: `limits.spawns` (workers per hour and per day) and `limits.turns` (turns per day), both
  required (§9).
- `resources` is required and applied (§8).

**One agent, many personas.** The owner's Hermes LXC on Discord works as one agent with many threads
and decides for itself what to save. The world and village residents follow that: personas, each on
its own thread, of **one** long-lived game agent, not one agent each. Nothing in core needs one
agent per resident. It costs persona isolation, which becomes a convention: one memory directory
and one thread per persona (§6, open questions). The ops agent is a separate long-lived agent,
because its powers differ.

### 3. Runtime: Claude Code first, pluggable later

A `runtime` field names what runs in the pod. Stage 1 knows only `claude-code`. Hermes and OpenClaw
come behind the same sidecar contract, after a spike (§10). Core stays the single control plane:
sessions, transcript, decisions, grants, events and traces. No orchestration framework runs in core.

### 4. Reach: everything through core, on the lease

A long-lived agent gets its own lease table, `standingMethods`: `definitionMethods` plus the methods
below. Each is also granted per definition through `sidecarTools`, as `PublishEvent` is
(`auth.go:173-176`). The egress lock does not change: the pod still reaches only core, the vault and
DNS.

- **Spawning workers: `CoreService.SpawnWorker(definition, input, idempotency_key)`.** Core runs
  `Invoke` in process. The principal is an in-process client (ADR-0066 §3, like the dispatcher's
  `bridge.yaml`): `definitions/clients/<agent>.yaml`, whose `definitions` list is the two-sided
  agreement on which workers the agent may start. The run is the client's, so `WaitForResult`,
  `maxParked` and the run cap apply unchanged. The agent reads results with
  `CoreService.WaitForResult` on the same lease.
- **Logs: `ViewLogs`**, which already exists (`auth.go:142`, component-scoped), joins the table.
- **Metrics: `CoreService.QueryMetrics`**, a read-only PromQL call over core's existing Prometheus
  client (`core/internal/coreserver/server.go:843`).
- **Issues: `CoreService.OpenIssue`.** Core hands it to a provisioner `PostIssue`, the same shape
  as `SubmitReview` → `PostReview` (ADR-0082 §9): the provisioner writes with the App token, and
  the pod holds none. Repos are limited to the definition's `owns.repos`.
- **Bridge events.** The agent's session has the fixed thread key `standing`. The dispatcher's
  `subscribes`/`owns` routing (ADR-0071 §4) appends each matched event to that one session, as core
  appends every event entry (ADR-0071 §3). The pod reads it on `WaitForMessages`. How far such an
  entry may act is §5.
- **Cluster reads.** The executor is unreachable and the pod has no service-account token.
  **Owner decision needed:**
  - (a) No cluster reads in stage 1. Alerts, Loki and Prometheus are the agent's view, and a worker
    with `cluster_access` (ADR-0037), spawned through `SpawnWorker`, does what needs `kubectl`.
  - (b) `CoreService.ClusterRead`: core relays a fixed set of read verbs to the executor under a
    read-only executor scope. This amends ADR-0037 (a second caller besides the shim) and needs core
    to hold an executor credential.
  - (c) The egress lock admits the executor, and the sidecar holds a read-only executor token, as
    it holds the vault token (§7). This amends ADR-0037 and ADR-0065 §5.
  - *Recommended:* (a). It needs no new credential, and (b) follows if (a) proves too slow.

### 5. Routed context informs; only a T0 or T1 cause acts

Amends ADR-0082 §1–§3. An event entry routed to a standing agent may start a turn: an ops agent
must react to an alert. It may not cause a side effect.

- **Each side-effect call carries its turn's cause.** The sidecar sends the `consumedSeq` of the
  turn-starting entry the turn took in. The worker already tracks it for its result entries
  (ADR-0070 slice 7). Core reads that entry's author and actor.
- **`SpawnWorker`, `OpenIssue` and `PublishEvent`** go through only when that entry is T0 (the
  owner: console, island, or a T0 label or @mention) or T1 (a fleet session's `PublishEvent`, with
  ADR-0082 §3's trust of its cause). Any other cause (a routed `subscribes` entry, an alert, a T2 or
  T3 text) gets `PermissionDenied`, keyed `agent.cause_untrusted`.
- **`DraftCard` is always open.** In an untrusted turn the agent drafts a card whose option is the
  spawn or the issue. The owner's answer is T0, and core runs the bound effect (ADR-0082 §6).
- Reading stays free, as ADR-0082 §3 holds: what the agent reads never narrows what it may *read*.
  Only effects take a cause.

### 6. Lifecycle: started once, then core keeps it up

Amends ADR-0029 and ADR-0048 §1–§2 for `standing` sessions only.

- **Start is explicit.** `AgentService.Start(definition)` (admin only) creates the `standing`
  session and appends a T0 start message, warm-then-append. A sync never starts one: ADR-0048 §1's
  human gate moves from every warm to the first. `Stop` is the human's off and archives nothing.
- **No idle timeout.** `ListIdleSessionIDs` skips `standing` sessions (`reconcile.go:326`).
- **Core re-warms on crash.** While a `standing` session is neither stopped nor archived, the 60 s
  reconcile re-warms it through `WarmIfIdle` when its phase is CRASHED or TERMINATED, at least
  `restart_after` after the terminal phase was written. The backoff is fixed at 5 min. It is a
  timestamp, not a counter: ADR-0048 §2's "no retry counter" stands. A crash loop shows as an alert
  on the existing pod-phase metrics, never a silent loop. The re-warm is core's command; k8s never
  restarts the agent.
- **A Job, not a Deployment.** It keeps the one pod-creation path, its golden tests and its revoke
  paths, and keeps `BackoffLimit: 0` (`pod.go:827`). A Deployment would let k8s restart the agent
  without core commanding it. A `standing` Job has no `activeDeadlineSeconds`.
- **The worker runs `lifecycle: resident`**, which never exits (ADR-0070 slice 7).
- **Context and transcript growth.** The context window is Claude Code's own: it auto-compacts, and
  the worker already records `compact_boundary` (`worker/src/session.ts:1015-1021`). The transcript
  rolls: once a week, at a turn boundary, core archives the session and opens a successor on the
  same definition with a T0 "continue" message. Postgres rows stay bounded, and the SDK resume state
  (per session) starts fresh. What the agent keeps across rolls is its memory directory, not the
  transcript.
- **Memory: a subPath on the shared PVC, not a new volume.** Claude Code's memory already lives in
  `CLAUDE_CONFIG_DIR=/claude-home`, a subPath of the shared RWX PVC (`pod.go:553`,
  `k8s/provisioner/pvc.yaml`). That PVC is on the default class, Longhorn, so it is already
  replicated. The agent gets `claude-home/agent-<name>/`, per agent rather than per session, so it
  survives rolls; with one directory per persona under it (§2). **Recorded risk:** Prometheus left
  Longhorn after a replica on `.165` fed it corrupt reads three times (infra-bootstrap #414,
  `gitops/platform/values/prometheus/values.yaml:190-193`). Control-plane nodes are under Longhorn
  memory pressure (infra-bootstrap ADR-0045:152). Memory files are few and small, and git-like
  recovery is not needed. A per-agent volume would add a second Longhorn volume with the same
  replica set and buy nothing, so the subPath is chosen. This replaces the first draft's
  "per-agent Longhorn PVC" (the owner's ask was replicated memory, which this keeps).

### 7. Credentials: a rotated file, not an extended token

Amends ADR-0065 §3–§5 and ADR-0075 §7 for `standing` pods.

- **The token leaves env.** A `standing` pod uses the sidecar's egress router (ADR-0075 §7), and
  the router reads its proxy URL from a file in a mounted Secret, `fleet-vault-<session>` (a whole
  volume, not a subPath, so kubelet updates it in place). It re-reads the file on each new tunnel.
  For a definition pod the router sends **every** host to the vault, so `unmatched_host_policy=deny`
  and the egress lock still decide. Unlike a repo pod, nothing goes direct. The worker's
  `HTTPS_PROXY` points at the router and carries no token.
- **The provisioner rotates on its own tick**, as it already refreshes owner vaults every 5 min
  (ADR-0075). For each listed `standing` Job it mints a new token with TTL 24h once the current one
  has under 12h left. It writes the Secret, then revokes the old label after 5 min, giving kubelet
  time to sync. `Mint` and `Revoke` are used as they are (`vault.go:587-640`); no extend is needed.
- **Expiry bound.** A token dies at most 24h after its last rotation. If the provisioner stops, or
  the Job is gone (core's Stop, archive or a crash), rotation stops, and the existing revoke paths
  (ADR-0065 §3's table) or the TTL end it.
- **Not a lease heartbeat.** A heartbeat flows from the pod to core and carries liveness state that
  core then trusts. This flows from the provisioner to the vault and to a file. It is triggered by
  a Job k8s lists, carries no liveness, writes no row, and involves neither the pod nor core.
  Liveness still comes only from core's reconcile of what k8s reports (ADR-0048 §2).
- **Revoke on widen** (`vault.go:430-470`) already deletes the definition's live Jobs (ADR-0065 §2).
  §6's re-warm then restarts the agent at the new pin with a fresh token. Nothing new is needed.

### 8. Capacity: its own slot, with real resources

`ReserveSlot` checks `MaxLive` before any inner pool (`core/internal/sessions/store.go:827`), and
today `MAX_IN_FLIGHT_TASKS=5`, definition 3, external 2 (`k8s/core.yaml:170-185`).

- **Prerequisite (stage 1, first PR):** apply a definition's `resources` to the worker container.
  `load.go:650` validates it; `pod.go` sets 1Gi/4Gi for everyone. The LimitRange pin test
  (`TestCreateWorkerPod_ResourcesWithinLimitRange`) covers both.
- **Numbers.** The game agent asks 512Mi and is limited to 1Gi; the ops agent the same. Two agents
  hold at most 2Gi at limit. Today's 5 workers hold up to 20Gi at limit, against worker-02's 16GB
  (infra-bootstrap `docs/infrastructure-actual.md:254`), so the limits are already oversubscribed.
  The standing pods are the small part.
- **Owner decision needed: the pool.**
  - (a) `MAX_STANDING_SESSIONS=2`, counted **outside** `MaxLive`. Workers keep all 5 slots, and
    `ReserveSlot` checks the standing pool alone for a `standing` session.
  - (b) `MAX_IN_FLIGHT_TASKS` 5 → 7, with the standing pool inside it. One cap, but every inner
    pool's arithmetic shifts.
  - (c) Carve 2 out of today's 5. Workers drop to 3.
  - *Recommended:* (a). It is the owner's "never starve workers", and the one new check sits in the
    same advisory-locked transaction.

### 9. Runaway: limits that bound loops, not cost

A spawn can publish, a publish can route back, and the agent reacts again. Stage 1 bounds that:

- **Spawn rate:** `limits.spawns` per hour and per day, checked by core in `SpawnWorker`, refused
  `ResourceExhausted` keyed `agent.spawn_limit`.
- **Depth:** a worker spawned by a long-lived agent starts at depth 1 of the existing 3
  (`maxPromptDepth`, ADR-0071 §2), and ADR-0071's cap of 3 agent wakes per thread per 24h applies to
  the `standing` thread. A worker cannot spawn.
- **Turn budget:** `limits.turns` per day. Past it, core appends no turn-starting entry and the
  agent's session shows a card instead.
- **Owner decision needed: the turn budget vs "no cost cap by design"** (`CLAUDE.md`, ADR-0004).
  - (a) Keep the turn budget as a loop guard: it counts turns, not money, and the owner raises it
    on a card.
  - (b) No budget; an alert on turns per hour only.
  - (c) Both: the budget, plus the alert at half of it.
  - *Recommended:* (c). The lock was about metered spend. A standing agent in a loop is a different
    failure.

### 10. Other runtimes: what the spike must answer first

The worker contract is about 8k lines of TypeScript (`worker/src/*.ts`): `canUseTool` and the fleet
gate, the non-blocking `{nb: 1}` stamp (`session.ts:698`), transcript shaping, and resume through
`RESUME_SESSION_ID` and `SaveAgentSessionId`. Hermes or OpenClaw must either reimplement that or sit
under it. The spike answers, in order:

1. **Auth.** Subscription OAuth is Claude Code's. A third-party harness likely needs a metered key,
   which conflicts with ADR-0004. If so, the spike stops there and the owner decides.
2. **The gate.** Does every tool call reach a fleet-owned `canUseTool`? Can a pending answer park
   with no pod?
3. **Sub-agents and schedulers.** Their own sub-agents and cron must route through core
   (`SpawnWorker`, Start); otherwise they bypass the control plane, which is alternative D.

Core and worker changes are required either way. This is not a config switch.

### 11. The game and ADR-0002

- **The fleet has no voice runtime.** A fleet-side runtime with no pod could only live in core,
  which is rejected alternative B. The first draft's stage 3 is removed.
- **Game-side, by owner direction (2026-11-16): start simple.** Repo residents are a direct LLM
  call on the game's voice path (`ukubi-game` `internal/voice`: today one turn on a small model, 160
  max tokens, 10 s, an in-memory daily budget, a metered key, no history: `anthropic.go:17-23`,
  `budget.go:8`). The game extends it with history per thread, persona and facts. It never escalates
  in v1: only the player's "Order an errand" verb starts a worker. Town, village and world talks go
  to Hermes or a fleet session directly, behind one game seam in `ukubi-game` `fleet/`.
- **Accepted in v1: two brains for a repo resident.** Its voice-path talk does not know what its
  fleet worker did, beyond the facts the game passes.
- **Embodiment** comes later, through the game's MCP; ADR-0083's shape for a remote MCP server is
  the related one.
- **ADR-0002 is reopened narrowly,** by its own revisit clause: runtimes inside the fleet, behind
  the sidecar and under core. A framework as a coordinator over the fleet stays rejected.

### 12. Stages, in order

Each is its own later ADR and PR.

1. **The standing role on Claude Code:** `resources` applied (first), the `standing` lifecycle and
   ceiling (§2), `standingMethods` and `SpawnWorker`/`QueryMetrics`/`OpenIssue` (§4), the cause rule
   (§5), Start, the idle exemption, re-warm and rolling (§6), the per-agent memory subPath (§6), the
   rotated token file (§7), the pool (§8), and the runaway limits (§9).
2. **The `runtime` field** and the Hermes/OpenClaw spike (§10).
3. **Live steering** through core: pause, redirect, goals, budget.

## Alternatives considered

- **A. Boring: resident definitions on Claude Code only, with rotation.** Rejected as the whole
  answer: an idle pod costs a worker's envelope, and a re-warmed resident reacts slowly. Partly
  kept: it is stage 1.
- **B. An agent loop inside core, no pod.** Rejected: an agent inside core's trust boundary, core is
  a single replica, and it is how a framework creeps in. It also rules out a fleet-side voice
  runtime (§11).
- **C. Two kinds, pluggable runtime behind the sidecar, core the only control plane.** Chosen and
  kept after the doubt cycle.
- **D. Hermes as an orchestrator outside the fleet, calling `AgentService`.** Rejected: two control
  planes, with needs, decisions, memory and traces split, and the game talking to two systems.
  (Town and world talk may still reach Hermes behind the game's own seam, §11; that is the game's
  choice, not a second fleet control plane.)
- **A Deployment for the standing pod.** Rejected in §6: k8s would restart it without core.
- **Extending the vault token in place.** Not possible: the vault has no extend (§7).
- **A scheduled rotation at about 11h** inside today's cap. Rejected by the owner.
- **A separate per-agent Longhorn PVC, or `local-path` plus S3.** Rejected in §6: the shared PVC is
  already Longhorn, and the subPath keeps one volume.

## Consequences

- A standing agent holds a pod all the time, in its own pool (§8).
- A token no longer always dies at 12h. It dies within 24h of the provisioner's last rotation.
- ADR-0048 §1's gate is per Start, not per warm, for `standing` sessions.
- An alert can make the ops agent think, never act. Every act from an untrusted cause is a card.
- `CLAUDE.md` and `DECISIONS.md` keep "no orchestration framework (Hermes/OpenClaw rejected)" until
  stage 2 is accepted.

## The bet

One control plane can host always-on agents. Exempting them from the 12h cap, with the provisioner
rotating the vault token into a file, keeps both rules that matter: no secret in the worker, and
core commands while the provisioner executes.

## Not doing, open questions

- **Not doing:** a framework over the fleet; an agent loop in core; a fleet voice runtime; a
  heartbeat; scheduled rotation; a Deployment.
- **Owner decisions needed:** cluster reads (§4); the pool (§8); the turn budget (§9).
- **Open:**
  - Hermes or OpenClaw, and whether either survives the spike's auth question (§10).
  - Which grants a long-lived ops agent may hold beyond §4's methods.
  - The game MCP contract.
  - The rollout order across the game's three scales.
- **Open: multi-character talk.** This covers group scenes, residents talking to each other,
  keeping characters apart, and routing who speaks. Starting direction:
  - one scene is one thread;
  - each output entry carries a `speaker` tag (persona id) that the game maps to a face and a
    bubble;
  - each persona keeps its own memory directory, and a persona reads only its own;
  - one director turn chooses the next speaker, rather than personas racing each other.
