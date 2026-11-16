# ADR-0084 — Long-lived agents: standing roles beside the workers, one control plane

- **Status:** Proposed (2026-11-16). Owner decision from an architect-companion interview the same
  day; owner review and a doubt cycle follow. Each stage below is its own later ADR and PR.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:** [ADR-0002](0002-no-orchestration-framework.md) (reopened by its own revisit
  clause, scope below), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3 (the 12h
  lifetime cap and the token per warm, for long-lived agents only),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7 (the same cap on the pod kind a
  long-lived agent would use), [ADR-0062](0062-defined-agents.md) §5 (a pool of its own).
- **Related:** [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §2 (no lease heartbeat:
  unchanged), [ADR-0064](0064-definition-pod-profile.md) (the definition pod),
  [ADR-0066](0066-agent-service.md) and [ADR-0070](0070-headless-invoke.md) (how a long-lived agent
  spawns workers), [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4 and
  [ADR-0078](0078-core-on-the-bridge.md) (`subscribes`, already anticipating "an always-on
  Hermes-type agent"), [ADR-0082](0082-decision-cards.md) (cards, grants, the wake rule),
  [ADR-0083](0083-remote-mcp-for-definitions.md) (the shape a game MCP would take),
  [ADR-0037](0037-thot-is-a-worker-task.md) (cluster access through the executor).

## Context

Every agent the fleet runs today is a worker: a session whose pod is warmed for a message, does the
work and goes. That fits work. It does not fit roles that should stand.

**The trigger is the game.** `ukubi-game` wants residents at three scales: a repo resident, a village
resident, and a world resident "like a Claude Code session in the ukubi folder". It wants the game's
MCP both ways (residents move, make faces) and CK-style character menus. The owner also wants
per-agent shape control, live steering, agents coordinating agents, control from the game, and the
event-driven direction the bridge started (ADR-0071, ADR-0078).

**What a resident costs today (measured 2026-11-16).** A resident talk is a full Claude Code worker
pod: 1Gi requested and 4Gi limit, fixed in the provisioner (`provisioner/internal/k8s/pod.go:173`,
`:792-800`) while about 240Mi is used, 3–4 s to warm, and the game shares `MAX_EXTERNAL_SESSIONS=2`
(`k8s/core.yaml:184`). A definition's `resources` is validated
(`core/internal/definitions/load.go:650`) but never applied: nothing reads it after the loader.
That is a known bug, fixed on its own, not by this ADR.

**The use is small.** The owner estimates a handful of short talks per evening, mostly status and
advice. The cost is in the shape, not the volume.

**Two caps stand in the way of a role that stays up.** A definition or repo pod dies at 12h
(`activeDeadlineSeconds`, ADR-0065 §3, ADR-0075 §7), and its vault token is minted per warm with a
TTL just past that cap. ADR-0002 rejected Hermes and OpenClaw for a proposer and critic pair, and
said to revisit "once N agents must run concurrently", not to override it silently. This is that
revisit.

## Decision

### 1. Two kinds of agents

- **Workers** are today's sessions: one pod per warm, single-shot or resident. They do the work and
  ask the owner through decisions (ADR-0082). Nothing about them changes.
- **Long-lived agents** hold a standing role. Examples: an ops agent that watches alerts, reads logs
  and read-only cluster state, reacts to bridge events, opens issues and spawns workers on a
  subject; the game's world and village residents.

### 2. Identity: a definition with a persona and a mandate

A long-lived agent is a reviewed definition (ADR-0062), one per role, carrying its persona and its
mandate. It is a new definition kind, validated against the same ceiling, and pinned the same way.

### 3. Runtime: pluggable, behind the sidecar contract

A `runtime` field names what runs in the pod: Claude Code, Hermes or OpenClaw. A time-boxed spike
chooses between Hermes and OpenClaw. Every runtime sits behind the existing sidecar contract, so
core stays the single control plane: sessions, transcript, decisions, grants, events and traces all
stay in core. No orchestration framework runs inside core.

### 4. Memory: the runtime's own files, on a replicated volume

The agent keeps its memory in the runtime's own memory files, on a per-agent **Longhorn
(replicated)** PVC (owner's choice). It survives the loss of a node; `local-path`, used today, pins a
volume to one node.

### 5. Senses: events and read-only domain access

- Bridge event subscriptions through the definition's `subscribes` (ADR-0071 §4). ADR-0082 §1
  stands: `subscribes` only routes, it never wakes. A long-lived agent is already running, so a
  routed event reaches it as context without a wake.
- Read-only access to its domain: Loki and Prometheus through core, the cluster through the
  executor (ADR-0037), repos by read.

### 6. Powers: set per definition

- **Default: watch, decide, delegate.** Spawn workers through `Invoke` (ADR-0070), open issues,
  draft cards (ADR-0082 §4), talk in the game.
- **Direct actions only through explicit grants** named in the reviewed definition, in the shape of
  ADR-0082's grants. Nothing else acts directly.

### 7. Lifetime: runs until stopped

The owner: "no point in rotating it." For this kind only:

- **No 12h lifetime cap** (amends ADR-0065 §3 and ADR-0075 §7).
- **The provisioner refreshes the vault token in place**, instead of one token per warm. The sidecar
  and its egress path hold the token (ADR-0075 §7's router); the agent holds no secret.
- **No heartbeat from the pod.** Core commands and the provisioner executes ([ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md)); liveness
  still comes from core's reconcile of what Kubernetes reports (ADR-0048 §2).
- **Its own capacity pool**, so long-lived agents never starve workers.

### 8. The game

- World and village residents are long-lived agents.
- Light talk goes through the game's existing voice path (`ukubi-game` `internal/voice`: the Messages
  API with a persona, facts and a budget). No pod.
- Errands are worker runs through `Invoke`.
- Embodiment comes later, through the game's MCP; ADR-0083's shape for a remote MCP server is the
  related one.

### 9. Stages, in order

Each is its own later ADR and PR.

1. **The long-lived role on the Claude Code runtime:** the definition kind, the capacity pool, the
   Longhorn memory volume, no lifetime cap, in-place token refresh, event subscriptions, workers
   spawned through `Invoke`.
2. **The `runtime` field** and the Hermes/OpenClaw spike, behind the sidecar contract.
3. **The voice runtime** (no pod) for light talk.
4. **Live steering** through core: pause, redirect, goals, budget.

### 10. ADR-0002 is reopened, narrowly

By its own revisit clause. The scope is runtimes inside the fleet, behind the sidecar and under
core. A framework as a coordinator over the fleet stays rejected (alternative D below).

## Alternatives considered

- **A. Boring: resident definitions on Claude Code only, with rotation.** Rejected as the whole
  answer: an idle pod costs a worker's envelope, and a re-warmed resident reacts slowly. Partly
  kept: it is stage 1.
- **B. An agent loop inside core, no pod.** Rejected: it puts an agent inside core's trust boundary,
  core runs a single replica, and it is how a framework creeps into core.
- **C. Two kinds, pluggable runtime behind the sidecar, core as the only control plane.** Chosen.
- **D. Hermes as an orchestrator outside the fleet, calling `AgentService`.** Rejected: two control
  planes, with needs, decisions, memory and traces split between them, and the game talking to two
  systems.
- **A scheduled rotation at about 11h**, inside today's cap. Rejected by the owner.
- **`local-path` plus an S3 backup** for memory. Rejected: the owner chose Longhorn.

## Consequences

- A long-lived agent holds a pod all the time. Its pool is the bound, and the workers' slots stay
  theirs.
- A token that never expires on its own replaces one that always did. Revoke on teardown still
  holds; what ADR-0065 §3 got from the TTL (a missed revoke ends at the cap) must come from the
  refresh stopping when core stops the agent.
- The Longhorn volume is the slow storage path ADR-0048 measured for many small files; memory files
  are few, so this is accepted.
- `CLAUDE.md` and `DECISIONS.md` say "no orchestration framework (Hermes/OpenClaw rejected)"; they
  change when stage 2 is accepted, not before.

## The bet

One control plane can host always-on agents of other runtimes. Exempting them from the 12h cap,
with the provisioner refreshing the token in place, keeps both rules that matter: no secret in the
pod, and core commands while the provisioner executes.

## Not doing, open questions

- **Not doing:** a framework over the fleet; an agent loop in core; a heartbeat; rotation.
- **Open:** Hermes or OpenClaw (the spike); the capacity pool's numbers; which grants a long-lived
  ops agent may hold; the game MCP contract; the rollout order across the game's three scales.
