# ADR-0084 — Long-lived agents: standing roles beside the workers, one control plane

- **Status:** Proposed (2026-11-16). Owner decisions from four architect-companion rounds the same
  day, after three doubt cycles and a survey of agents on Kubernetes. This ADR records decisions and
  invariants only. Mechanisms live in the stage-1 build slices (appendix); each slice is designed
  and doubted against the code when it is built.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:**
  - [ADR-0002](0002-no-orchestration-framework.md): reopened narrowly, by its own revisit clause (§12).
  - [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) and
    [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1–§2: a standing pod is started once
    and kept up by core (§6).
  - [ADR-0037](0037-thot-is-a-worker-task.md): a standing pod may hold its own read-only kube
    identity (§4).
  - [ADR-0062](0062-defined-agents.md) §5/§7: the `standing` lifecycle, its ceiling and its pools
    (§2, §9).
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§5 and
    [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7: the 12h cap and the token in
    env, for standing pods only (§8); the egress lock admits the API server for them (§4).
  - [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4–§5 and
    [ADR-0082](0082-decision-cards.md) §1–§3: a subscription wakes a standing agent, threads are
    per subject, and what a turn may cause (§5).
  - `CLAUDE.md`: "cap 5 = max warm pods" becomes "5 worker pods, plus the standing pool".
- **Related:** [ADR-0004](0004-subscription-oauth-not-metered-api.md) (no cost cap: §10),
  [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core commands, the provisioner executes),
  [ADR-0068](0068-islands.md) (the island in the game: §11), ADR-0085 (overlay session volumes and
  the warm pool, proposed alongside).

## Context

Every agent the fleet runs today is a worker: a session whose pod is warmed for a message, does the
work and goes. That fits work. It does not fit roles that should stand: an ops agent that watches
alerts, reads logs and metrics, opens issues and sends workers; and the game's host, which speaks
for the world and village residents.

Three doubt cycles on an earlier, mechanism-level draft of this ADR kept finding mechanism bugs (a
migration CHECK, a resume cursor, mount ownership, a cause window merged across inputs), never a
wrong decision. The owner split the two: this ADR holds the decisions, and the build slices hold the
mechanisms, with every cycle-3 finding as an acceptance test.

A survey of agent runtimes on Kubernetes (kagent, agent-sandbox, Hermes, OpenClaw, HolmesGPT,
eventing-lab, Dapr Agents and others, checked 2026-11-16) found nothing that replaces this design:
kagent's dedicated agent bindings are deferred and it needs its own Substrate runtime; Hermes and
OpenClaw keep credentials inside the agent; HolmesGPT's pods hold credentials and its alert triggers
are planned only. Ideas taken from it are named where they land.

## Decision

### 1. Two kinds of agents

- **Workers** are today's sessions: one pod per warm, single-shot or resident. Nothing about them
  changes.
- **Long-lived agents** hold a standing role and run until a human stops them. Stage 1 has two: an
  **ops agent** and a **game host**.

### 2. Identity and ceiling

A long-lived agent is a reviewed definition (ADR-0062) with a persona and a mandate, and the
lifecycle value `standing`. The loader enforces: mode `auto` or `default` (never `plan`, which would
wait on an approval forever); no idle timeout; `limits.spawns` and `limits.turns` required (§10);
`resources` required and applied (today a definition's `resources` is validated and never applied,
`core/internal/definitions/load.go:650`; fixing that is stage 1's first slice). A standing-only
definition is never opened by `Open`, `Invoke` or a routed proposal: only `Start` (§6).

### 3. One pod per agent, one session per thread

- **One pod per agent.** It runs the agent's runtime (Claude Code in stage 1) behind the sidecar
  contract. Core stays the single control plane.
- **One SDK session per thread.** A thread is a subject: an Alertmanager alert group, a game scene,
  the owner's admin thread. Each thread is its own SDK session inside the agent's process, FIFO
  within the thread, with a small cap on threads running at once. Threads share the agent's memory
  (§7) and nothing else.
- **Trust is per thread** (§5). An untrusted alert and the owner's message never share a context,
  so a cause can never be laundered across inputs inside one turn.
- **Scale vertically first** (a larger envelope), horizontally (a second pod of the same agent,
  threads partitioned) only when one pod is not enough.
- Like the owner's Hermes agent on Discord: one agent, many threads.

### 4. Reach: everything through core, plus a read-only kube identity

- **Through core, on the lease:** `SpawnWorker` (an in-process `Invoke` of a worker definition that
  lists the agent's client, depth 1), `WaitForResult` for its own runs, `ViewLogs` (already on the
  lease, `core/internal/coreserver/auth.go:142`), `QueryMetrics` (read-only PromQL), and
  `OpenIssue` (handed to the provisioner, like `SubmitReview`). Each is granted per definition.
- **Cluster reads: the pod's own ServiceAccount**, bound to a read-only ClusterRole: the read rules
  of `thot-executor` (infra-bootstrap `gitops/platform/thot/rbac.yaml`) with no secrets, no RBAC
  objects, no `patch` or `delete`. RBAC is the boundary, not a verb allowlist: today's executor
  token can also mutate, and its `read_only` flag is set by the caller
  (`executor/cmd/executor/main.go:41`). This amends ADR-0037's "zero kube credentials in a pod" for
  standing agents only, and the egress lock admits the API server for standing pods only. The
  ServiceAccount and its binding live in infra-bootstrap's gitops, human-reviewed; the provisioner
  only names it.

### 5. Events and provenance

- **Alerts reach the bridge first** (ADR-0071's outstanding item): without it no alert reaches a
  standing agent.
- **A subscription wakes a standing agent** (amends ADR-0082 §1 for `standing` definitions only).
  The event lands on its thread, created on first use. For alerts the thread key is Alertmanager's
  `groupKey`, so one alert group is one thread and its repeats land in it.
- **Damping, twice.** A dedicated Alertmanager route to the bridge groups and rate-limits
  (`group_wait` 30s, `group_interval` 5m, `repeat_interval` 4h). Core adds a wake cooldown per
  thread, in case a route is misconfigured.
- **Causes.** Reading is free. An effect (`SpawnWorker`, `OpenIssue`, `PublishEvent`) needs a T0
  or T1 cause, computed by core from its own transcript for that thread: T0 is the owner (console,
  island, a T0 label or mention), T1 a trusted fleet `PublishEvent`. A routed alert, a game guest's
  text and a core-authored entry are untrusted. An empty window is untrusted. In an untrusted turn
  the agent drafts a card (ADR-0082 §6), and the owner's answer is T0.
- **Speech is not an effect.** The game host's replies go out on a separate speech channel
  (§11). Speech is exempt from the cause rule and is never routed as a wake, so it can neither loop
  nor reach another agent.
- **Accepted residual.** A trusted turn still reads what memory holds (§7). The rule bounds what
  starts an effect, not what shapes it.

### 6. Lifecycle: started once, kept up by core

Amends ADR-0029 and ADR-0048 §1–§2 for `standing` sessions only.

- **Start is explicit and admin-only.** It moves ADR-0048 §1's human gate from every warm to the
  first. `Stop` is the human's off.
- **Kept up by core.** Standing sessions are exempt from the idle and startup-stall sweeps. Core's
  existing re-warm takes a standing session that is not stopped back up after a crash or a
  termination, after a back-off. That is a timestamp, not a retry counter (ADR-0048 §2 stands);
  liveness still comes only from core's reconcile.
- **Replaced, not rotated, on a pin change**, at a turn boundary, with only the memory carried
  over. No weekly roll: the owner sees no point in rotating a role that works.
- **A Job, until the spike decides.** The standing pod stays a provisioner Job (one pod path,
  its golden tests and revoke paths). A time-boxed spike runs one definition pod as an
  agent-sandbox `Sandbox` (kubernetes-sigs/agent-sandbox, with our Cilium policy and its
  NetworkPolicy set to `Unmanaged`), to see whether reconcile, revoke and rotation port cleanly.
  The workload choice is made after the spike, in its own ADR.

### 7. Memory: the agent's own files, versioned

- **One memory directory per agent**, on replicated storage (Longhorn), carried across
  replacements. Everything else (SDK resume state, genome) stays per session.
- **Written freely.** The agent decides what to save (MEMORY.md and USER.md, Hermes-style). The
  memory directory is a git repository: one commit per write, tagged with the thread and its trust
  tier. The console shows the history as a diff and reverts a commit in one action.
- **Why free:** the real risk is a note saved from hostile text in an untrusted thread and acted on
  later in a trusted turn. Today it is rare (owner only, own infrastructure) and bounded (a spawned
  worker still runs under its own ceiling). Versioning makes it visible. **Revisit** when public
  input (game guests, public issues) reaches an agent: memory writes from untrusted threads then go
  through a card.

### 8. Credentials: no secret in the pod, and no 12h cap

Amends ADR-0065 §3–§5 and ADR-0075 §7 for `standing` pods only.

- The pod still holds only placeholders. The vault token leaves env: the provisioner rotates it
  into a file the sidecar's egress router reads, on its own tick, and revokes the old label after a
  grace. Teardown revokes both labels.
- This is not a lease heartbeat: it flows from the provisioner to the vault and k8s, carries no
  liveness and writes no row.

### 9. Capacity

- **Standing pool:** `MAX_STANDING_SESSIONS=2`, outside the cap of 5 and outside the definition and
  external pools. A standing session is not counted by the live, definition, external or per-owner
  counts. Workers keep all five slots.
- **Spawn pool:** a per-agent sub-pool, `limits.spawns.concurrent` (default 1), inside the
  definition pool. A busy agent holds at most that many workers, the game's external slots are
  untouched, and admin definition runs keep a slot.
- **Size:** request 1Gi, limit 2Gi, applied from the definition.

### 10. Runaway

- **Spawns:** `limits.spawns` per hour and per day, depth 1. `PublishEvent` keeps ADR-0071's hop
  limit.
- **Turns:** a daily budget, `limits.turns`. It counts turns, not money, so ADR-0004's "no cost cap"
  stands: a loop is a different failure from metered spend. Past the budget, new entries queue
  without starting a turn, and core drafts one card asking to raise it.
- **Alert** when the hourly turn rate would use half the daily budget, so a loop is seen before it
  hits the wall.

### 11. The game

- **Light resident talk** stays on the game's own voice path (`ukubi-game` `internal/voice`), no
  pod.
- **The host's talk goes over the bridge.** The game publishes talk events; the host's scene threads
  take them in; the host replies on the speech channel (§5), which the game renders as bubbles and
  faces. Stage 1 is speak only.
- **The owner's admin talk in the game is a direct T0 channel:** the fleet island
  (`<fleet-agent>`, ADR-0068) embedded in the game page, on the owner's own fleet cookie. The game
  server never vouches for the owner.
- **Later:** multi-character scenes (who speaks, keeping characters apart), embodiment through the
  game's MCP (ADR-0083's shape).

### 12. Runtimes and ADR-0002

Stage 1 runs Claude Code only. A `runtime` field and a Hermes/OpenClaw spike are later, behind the
same sidecar contract; the spike must first answer auth (subscription OAuth is Claude Code's), the
gate (every tool call through a fleet-owned `canUseTool`) and sub-agents (through core). ADR-0002
is reopened only for runtimes inside the fleet, under core. A framework over the fleet stays
rejected.

### 13. Stages

1. **Both agents, thin.** Ops: logs, metrics, alerts, issues, spawns, cluster reads. Game host:
   speak only. Built as the slices in the appendix.
2. **The `runtime` field** and the Hermes/OpenClaw spike.
3. **Live steering** through core: pause, redirect, goals, budget.

## Alternatives considered

- **Resident definitions only, re-warmed per talk.** Rejected as the whole answer: an idle-to-warm
  resident reacts in seconds, and a role loses its context at every teardown.
- **An agent loop inside core, no pod.** Rejected: an agent inside core's trust boundary, and how a
  framework creeps in. The survey's "loop in the control plane, tools in the pod" designs (Coder
  Agents, Managed Agents) were closed for the same reason, and because Claude Code's tools run in
  its own process against the local filesystem.
- **Hermes or OpenClaw as an orchestrator outside the fleet.** Rejected: two control planes.
- **One merged thread per agent** (the earlier draft). Rejected: it merges trusted and untrusted
  inputs into one context, which is the cause-window blocker.
- **A pod per thread.** Rejected: today's worker model with a shared memory directory, at a pod per
  scene.
- **Cluster reads through the executor** (a read-only token, or core relaying reads). Rejected: the
  executor's identity can mutate, so the boundary would be a verb allowlist, not RBAC; core would gain
  an executor credential.
- **Cards for every memory write.** Rejected for now (§7): a daily tap for a risk that is small today.

## Consequences

- Two standing pods run all the time, in their own pool, at up to 4Gi beside the workers.
- ADR-0048 §1's gate is per Start, not per warm, for standing sessions.
- An alert can make the ops agent think, never act; an act from an untrusted cause is a card.
- A standing pod holds a read-only kube identity, the first pod in the fleet with one.
- `CLAUDE.md` and `DECISIONS.md` keep "no orchestration framework (Hermes/OpenClaw rejected)" until
  stage 2 is accepted.

## The bet

One control plane can host always-on agents. One pod per role, carrying a session per thread, is
cheap enough at two pods, and safe enough because trust is computed per thread by core, effects need
a trusted cause, and RBAC (not an allowlist) bounds what the pod can read.

## Not doing

A framework over the fleet; an agent loop in core; a fleet voice runtime; a Deployment; a weekly
roll; a lease heartbeat; a shared writable directory between agents. Later, not now: multi-character
scenes, other runtimes, narrower GitHub scope for repo pods, gVisor.

## Appendix: stage-1 build slices

Each slice is its own PR. Its plan checks every claim against the code and is doubted before the
build. The tags are doubt-cycle-3 findings (2026-11-16) that the slice must pass as acceptance tests.

1. **`resources` applied** from the definition to the worker container (today a fixed 1Gi/4Gi in
   `provisioner/internal/k8s/pod.go`). Prerequisite for everything below.
2. **Alertmanager onto the bridge**, with the dedicated route (§5).
3. **The `standing` lifecycle, ceiling and Start.**
   - Migration (next free number, 000027): `standing` in `sessions.lifecycle`'s CHECK by dropping
     and re-adding the constraint, never by editing 000015. A standing row must satisfy 000015's
     `sessions_invoker_owns` (`invoker IS NULL OR owner = 'client:' || invoker`): the owner is the
     standing principal's client, and the admin who started it is kept in another column.
   - `Open`, `Invoke` and routed proposals refuse a standing-only definition
     (`core/internal/dispatch/dispatch.go:706` opens a thread per subject today).
4. **Threads and resume.** One SDK session per thread in the worker; core keeps a cursor per
   standing thread, so entries appended while the pod is down are delivered after the re-warm (today
   `resumeFromSeq` is `LatestSeq` at dispatch, so they would fall below it). Amends the "append before
   the pod exists" forbidden pattern for standing threads only.
5. **Kept up by core.** Sweep exemptions; the back-off timestamp is set on **every** terminal path,
   including `ReportPodEvents` and a failed warm (or a NULL counts as due).
6. **Provenance per thread.** The cause is the least trusted turn-starting entry in the thread's
   window, the window starts at the result before the previous one (the pod-written `consumedSeq`
   is a high-water mark raised at intake, `worker/src/session.ts:2012`), and an empty window is
   untrusted. The speech channel's exemption.
7. **Capacity.** The standing pool; standing excluded from the live, definition, external and
   per-owner counts in `ReserveSlot` (`core/internal/sessions/store.go:807`, the external check at
   `:879`); the per-agent spawn sub-pool.
8. **Memory.** The per-agent directory, created owned by uid 1000 before the mount (a missing subPath
   is created root-owned, which breaks the SDK's writes), with a golden test; the git versioning and
   the console diff.
9. **Credentials.** The rotated token file; the next label is written on the Job before the mint, so
   a crash between mint and patch never leaves an unrevocable token.
10. **Reach.** `SpawnWorker`, `WaitForResult`, `QueryMetrics`, `OpenIssue`; the read-only
    ServiceAccount (infra-bootstrap) and the egress rule for the API server.
11. **Runaway.** Spawn limits, the turn budget, its card and its alert; the per-thread wake cooldown.
12. **The game host, thin:** talk events over the bridge, scene threads, the speech channel.
