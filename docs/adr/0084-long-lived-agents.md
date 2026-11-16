# ADR-0084 — Long-lived agents: standing roles beside the workers, one control plane

- **Status:** Proposed (2026-11-16). Owner decisions from four architect-companion rounds the same
  day, after three doubt cycles on an earlier mechanism-level draft, a survey of agents on
  Kubernetes, and one doubt cycle on this decision-level rewrite. Choices marked **default, owner may
  change** are the coordinator's defaults; **owner to confirm** marks a consequence the owner has not
  yet seen. Mechanisms live in the stage-1 build slices (appendix), each designed and doubted
  against the code when it is built.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:**
  - [ADR-0002](0002-no-orchestration-framework.md): reopened narrowly, by its own revisit clause (§13).
  - [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) and
    [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1–§2: a standing pod is started once
    and kept up by core (§7); a standing thread may be appended to before its pod exists (§3).
  - [ADR-0058](0058-an-answer-wakes-the-session.md) and `DECISIONS.md` §2 ("appending to a session
    that has no live pod without warming first"): not forbidden for standing threads, which keep
    their own cursor (§3).
  - [ADR-0037](0037-thot-is-a-worker-task.md): one standing pod may hold its own read-only kube
    identity (§5).
  - [ADR-0062](0062-defined-agents.md) §5/§7: the `standing` lifecycle, its ceiling and its pools
    (§2, §10).
  - [ADR-0064](0064-definition-pod-profile.md)'s method table (`ViewLogs` is forbidden to
    definitions, "fleet-wide Loki"): allowed to standing definitions that are granted it (§5).
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3–§5 and
    [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7: no credential in any pod's env,
    fleet-wide (§9); no 12h cap for standing pods; the egress lock admits the API server for the one
    pod with a kube identity (§5).
  - [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4–§5 and
    [ADR-0082](0082-decision-cards.md) §1–§3: a subscription wakes a standing agent, threads are
    per subject, cards are budgeted per tier (§6).
  - `CLAUDE.md`: "cap 5 = max warm pods" becomes "5 worker pods, plus the standing pool".
- **Related:** [ADR-0004](0004-subscription-oauth-not-metered-api.md) (no cost cap: §11),
  [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core commands, the provisioner executes),
  [ADR-0066](0066-agent-service.md) (registered clients, the game's connection: §12).

## Context

Every agent the fleet runs today is a worker: a session whose pod is warmed for a message, does the
work and goes. That fits work. It does not fit roles that should stand: an ops agent that watches
alerts, reads logs and metrics, opens issues and sends workers; and the game's host, which speaks
for the world and village residents.

Three doubt cycles on an earlier, mechanism-level draft found mechanism bugs, never a wrong decision,
so the owner split the two: this ADR holds decisions, and the build slices hold mechanisms. A survey
of agent runtimes on Kubernetes (2026-11-16) found nothing that replaces this design. A doubt cycle on
the rewrite then showed that trust per thread cannot be enforced inside one process under one lease:
every thread shares Bash and loopback to the sidecar. Trust is therefore per pod (§4).

## Decision

### 1. Two kinds of agents

- **Workers** are today's sessions: one pod per warm. Nothing about them changes, except that no
  credential sits in a pod's env any more (§9), which is fleet-wide.
- **Long-lived agents** hold a standing role and run until a human stops them. Stage 1 has two: an
  **ops agent** and a **game host**.

### 2. Identity and ceiling

A long-lived agent is a reviewed definition (ADR-0062) with a persona, a mandate and the lifecycle
value `standing`. The loader enforces: mode `auto` or `default` (never `plan`); no idle timeout;
`limits.spawns` and `limits.turns` required; `resources` required and applied (today validated and
never applied, `core/internal/definitions/load.go:650`). A standing-only definition is reached only
through `Start` (§7) and its own methods (§6, §12), never `Open`, `Invoke` or a routed proposal
(`core/internal/dispatch/dispatch.go:713` calls `Open` today).

### 3. Data model: one row per tier, threads inside it

**Default, owner may change.**

- **One `sessions` row per pod tier** (§4): `invoker = 'standing'`, `owner = 'client:standing'`,
  `thread_key` = `trusted` or `untrusted`. That satisfies migration 000015's
  `sessions_invoker_owns` and `sessions_thread_key_invoker` (`thread_key IS NULL OR invoker IS NOT
  NULL`, 000015:32), and the existing unique `(invoker, definition_name, thread_key)` index makes
  each tier unique per agent. The admin who started it is kept in a new `started_by` column.
- **Threads inside the row.** `transcript` and `decisions` entries gain a `thread_key` (the thread's
  key, not the tier). `decisions`' `UNIQUE (session_id, request_seq)` still holds: the sequence stays
  per session.
- **Per thread, per pod:** a map from thread to its SDK session id, and a cursor (`consumedSeq`). Core
  stores both (a `standing_threads` table) and hands them to the pod at warm, so an entry appended
  while the pod was down is delivered after the re-warm. This is why the "append before warm"
  prohibition (DECISIONS §2, ADR-0058, ADR-0048's `resumeFromSeq = LatestSeq` at dispatch) does not
  apply to standing threads. The cursor drives delivery only, never trust (§6).
- **Migrations:** the `standing` value in `sessions.lifecycle`'s CHECK (drop and re-add the
  constraint in a new migration, never by editing 000015), `started_by` and `rewarm_after` on
  `sessions` (§7), `thread_key` on `transcript` and `decisions`, and `standing_threads`.

### 4. Trust is per pod: one pod per tier for each agent

**Owner decision.**

- **The trusted pod** holds the owner's threads: the admin thread and the owner's game talk (§12).
  It has Bash. Its effects (`SpawnWorker`, `OpenIssue`, `PublishEvent`) still need a T0 or T1 cause
  in the calling thread (§6).
- **The untrusted pod** holds alert groups and game scenes with guests. It has no Bash, and its lease
  carries no effect method at all: it reads, drafts cards, speaks (§8) and writes `untrusted/`
  memory (§9). An injected thread there can at most draft a card or write a labelled note.
- **Each pod has its own lease and its own `sessions` row** (§3). The two share memory only through
  their sidecars, by tier (§9).
- **Core routes every entry to a tier:** T0 and T1 entries to the trusted pod, everything else to
  the untrusted pod. A thread belongs to one tier for its whole life.
- **Consequence: the standing pool counts pods.** `MAX_STANDING_SESSIONS` defaults to 4 (two agents
  × two tiers). **Owner to confirm.**

### 5. Reach: through core, plus one read-only kube identity

- **Through core, on the lease:** `SpawnWorker` (an in-process `Invoke` of a worker definition that
  lists the agent's client, depth 1), `WaitForResult` for its own runs, `ViewLogs`, `QueryMetrics`
  (read-only PromQL), and `OpenIssue` (handed to the provisioner, like `SubmitReview`; repos limited
  to the definition's `owns.repos`). Each is granted per definition and per tier (§4).
  - `ViewLogs` is in `unboundMethods` today (`core/internal/coreserver/auth.go:138-142`), not
    `definitionMethods` (`:159`), and ADR-0064 forbids it to definitions as fleet-wide Loki. A
    standing definition granted it may call it (amends ADR-0064).
- **Cluster reads: a ServiceAccount, granted per definition.** Only the ops agent holds one, bound to
  a read-only ClusterRole: the read rules of `thot-executor` (infra-bootstrap
  `gitops/platform/thot/rbac.yaml`, including `pods/log`) with no secrets, no RBAC objects, no
  `patch` or `delete`. RBAC is the boundary, not a verb allowlist: the executor's own identity can
  mutate, and its `read_only` flag is set by the caller (`executor/cmd/executor/main.go:44`). The
  game host gets none.
- **It goes to the ops agent's untrusted pod**, the one running the alert threads, because alert
  triage is where cluster state is read. That pod has no Bash and no effect method, so what it reads
  can leave only as a card the owner reads or as a labelled `untrusted/` note. The token is mounted
  in its sidecar only (reads go through a sidecar tool), never in the worker container. Reading
  `pods` is safe once no pod spec holds a credential (§9, a prerequisite). The egress lock admits the
  API server for this pod only. The ServiceAccount and its binding live in infra-bootstrap's gitops,
  human-reviewed; the provisioner only names it. This amends ADR-0037's "zero kube credentials in a
  pod" for that one pod.

### 6. Events, threads and provenance

- **Alerts reach the bridge first** (ADR-0071's outstanding item). The legacy `/webhook/alertmanager`
  path is retired in the same slice, so no alert is delivered twice.
- **A subscription wakes a standing agent** (amends ADR-0082 §1 for `standing` definitions only).
  Events append to the existing standing rows; none opens a session, so ADR-0071's cap of three
  event-opened sessions and its "an alert storm cannot spawn a pod" still hold.
- **Thread keys.** An alert group's key is a stable hash of its group label set (Alertmanager's
  `groupKey` also embeds the route, which can change); a scene is `scene:<id>`; the admin thread is
  `admin`. **Default, owner may change.**
- **Thread lifecycle.** A thread retires when its alert resolves, its scene ends, or after 7 days
  idle. Its SDK session is dropped; a later event with the same key starts a fresh thread.
  **Default, owner may change.**
- **Damping, twice.** A dedicated Alertmanager route to the bridge groups and rate-limits
  (`group_wait` 30s, `group_interval` 5m, `repeat_interval` 4h). Core adds a wake cooldown per
  thread: events inside the window queue and are coalesced into one entry at its end, never dropped,
  and a resolved event always passes.
- **Causes.** Reading is free. In the trusted pod an effect needs a T0 or T1 cause, computed by core
  from its own transcript for the calling thread: T0 is the owner, T1 a trusted fleet
  `PublishEvent`. The window starts at the result before the previous one (the pod-written
  `consumedSeq` is a high-water mark, `worker/src/session.ts:2012`), and an empty window is
  untrusted. Otherwise the agent drafts a card (ADR-0082 §6), and the owner's answer is T0.
- **Cards, per tier.** The card budget (today 10 per definition per 24h,
  `core/internal/coreserver/card.go:59`) is split by tier, so untrusted threads cannot spend the
  trusted pod's. A standing card's subject is `standing/<agent>/<tier>/<thread hash>`, which keys
  `decisions_one_open_card_idx` (`lower(subject)`, 000025:24). **Default, owner may change.**
- **Accepted residual.** A trusted turn still reads `untrusted/` notes (§9). The rule bounds what
  starts an effect, not what shapes it.

### 7. Lifecycle: started once, kept up by core

Amends ADR-0029 and ADR-0048 §1–§2 for `standing` sessions only.

- **Start is explicit and admin-only**, and creates both tier rows. It moves ADR-0048 §1's human
  gate from every warm to the first. `Stop` is the human's off.
- **Kept up by a new re-warm.** Today's re-warm (`ListRewarmRuns`) covers single-shot runs in their
  exit window only (`core/internal/sessions/store.go:498-502`, `reconcile.go:393-405`). Standing
  sessions get their own selector, keyed on a new `sessions.rewarm_after` column that every terminal
  path sets (through `SetPodPhase`, `store.go:1062`, which also covers `ReportPodEvents` and a failed
  warm); a NULL counts as due. They are exempt from the idle and startup-stall sweeps. It is a
  timestamp, not a retry counter (ADR-0048 §2 stands). **Default, owner may change.**
- **Crash loop.** After 3 consecutive back-offs, core stops re-warming that pod and alerts the owner
  (§11's route), until a human starts it again. **Default, owner may change.**
- **Replaced on a pin change by a drain**: core stops intake on every thread (new entries are held,
  not lost), waits for in-flight turns up to a deadline, then forces the replacement. Only memory
  carries over. **Revocation and widening** (the vault's revoke-on-widen) **tear down at once.** No
  weekly roll. **Default, owner may change.**
- **A Job, until the spike decides.** The standing pod stays a provisioner Job. A time-boxed spike runs
  one definition pod as an agent-sandbox `Sandbox` (kubernetes-sigs/agent-sandbox, our Cilium policy
  kept, its NetworkPolicy `Unmanaged`); the workload choice follows in its own ADR.

### 8. Speech

- **A core method, granted per definition**: `CoreService.Speak(speaker, text, face)`, lease-bound,
  bound to the calling thread's own scene (`thread_key = scene:<id>`). Core publishes it to the bridge
  as `dev.bnei.fleet.speech` with that scene as subject.
- **Speech is never a wake** (the dispatcher drops the type) and is exempt from the cause rule. **The
  game never republishes host speech as talk.**
- **Either pod's scene threads may speak, each into its own scene only.** A guest scene lives in the
  untrusted pod, which never sees `trusted/`, so trusted memory cannot be spoken to a guest. The
  trusted pod's scenes are the owner's talk, and the game renders them to the owner only.
- Only the game host holds the grant; the ops agent cannot speak.

### 9. Credentials and memory, by tier

- **No credential in any pod's env, fleet-wide** (owner decision; amends ADR-0065 §4 and ADR-0075
  §7). Today a definition worker's `HTTPS_PROXY` carries the vault token
  (`provisioner/internal/k8s/pod.go:248-251`, `:312`), the sidecar of a repo pod gets it as
  `FLEET_VAULT_PROXY` (`:845`), and every pod carries `LEASE_ID` (`:485`). Any identity that can `get
  pods`, such as `thot-executor` today, reads them. The provisioner instead writes the per-warm vault
  token and the lease into a per-pod Secret, mounted as a file in the sidecar only, with an
  `ownerReference` on the Job. Env holds only paths. The worker reaches the vault through the
  sidecar's egress router, which today serves repo pods only (`pod.go:841`). A prerequisite slice,
  before any ServiceAccount exists.
- **Standing pods: rotation, no 12h cap.** The provisioner rotates the token into that file on its own
  tick, under `lockVault` (`provisioner/internal/grpcserver/server.go:924`). The Job carries two
  annotations, `vault-label` and `vault-label-prev`; the next label is written before the mint, and
  the old one is revoked after a grace. Teardown revokes both. Not a lease heartbeat: it carries no
  liveness and writes no row.
- **Memory, by tier** (owner decision):
  - `trusted/` is read and written by the trusted pod.
  - `untrusted/` is written by the untrusted pod and readable by both; the trusted pod sees it
    labelled as untrusted notes.
  - The untrusted pod never sees `trusted/`.
  - The sidecar enforces the path rule. Each directory is a git repository with one writer, its
    pod's sidecar, which makes the commits (one per write, tagged with thread and trust tier) and
    serializes them. The git directory is never in the worker's writable mount. The console shows
    the history as a diff and reverts a commit.
  - On replicated storage (Longhorn), carried across replacements. The shared RWX volume ignores
    the pod's `fsGroup` (`pod.go:567-579`): Longhorn's CSIDriver has `fsGroupPolicy:
    ReadWriteOnceWithFSType` (checked on the cluster, 2026-11-16), so the provisioner creates each
    directory owned by uid 1000 before the mount.

### 10. Capacity

- **Standing pool:** `MAX_STANDING_SESSIONS=4` pods (§4, owner to confirm), outside the cap of 5 and
  outside the definition and external pools. Standing rows are excluded from the live, definition,
  external and per-owner counts in `ReserveSlot` (`store.go:807`, the external check at
  `:879-881`).
- **Spawn pool:** spawned workers are owned by the agent's client (`invoke.go:107-108` sets the
  owner), so today they would fall into `MaxExternal`. They are excluded from `externalLive` and
  `MaxExternal` and counted in a per-agent sub-pool, `limits.spawns.concurrent` (default 1), under a
  fleet-wide spawned-session cap inside `MAX_IN_FLIGHT_TASKS`. It works even with
  `MAX_DEFINITION_SESSIONS=0`, today's default (`core/internal/config/config.go:347`).
  **Default, owner may change.**
- **Threads:** at most 2 running at once per pod. Memory is sized per running thread from today's
  worker ceiling of 4Gi (`pod.go:173`): a request of 1Gi and a limit of 4Gi per running thread, so up
  to 8Gi per pod at the limit. **Open risk:** four standing pods at the limit is 32Gi beside the
  workers, and concurrent CLI streams share one subscription; its headroom is unmeasured.

### 11. Runaway

- **Spawns:** `limits.spawns` per hour and per day, depth 1. `PublishEvent` keeps ADR-0071's hop limit.
- **Turns:** a daily budget per agent across both pods, `limits.turns`, on a calendar day in
  Europe/Paris. It counts turns, not money, so ADR-0004's "no cost cap" stands. Core gates delivery
  per thread: past the budget, a new non-T0 entry is held (delivered after the reset) and core drafts
  one card asking to raise it. **T0 entries bypass the budget.**
- **Alert** when `increase(turns_total[1h]) * 24 >= 0.5 * limits.turns` for 15 minutes, per agent.
- **Runaway and standing-pod alerts go to the owner, never to the agent's route**, so an agent that is
  running away is not woken by its own alert. **Default, owner may change.**

### 12. The game

- **Light resident talk** stays on the game's own voice path (`ukubi-game` `internal/voice`), no pod.
- **The game has its own connection to core** as a registered `AgentService` client (ADR-0066), and
  its own system, sharing UI components with the fleet. It is not the island, and it never uses `Open`.
- **It addresses a standing thread with `AgentService.PostToStanding(definition, thread_key, text,
  actor)`.** Authorization: the client's file lists the definition in a `standing` grant and the
  definition lists the client in `invokers`. The entry is T0 only when the game authenticated the
  player through authentik OIDC and core finds that actor on the `ukubi-game` client's
  `actors.allow`; then it goes to the trusted pod. Every other actor (guests) is untrusted and goes to
  the untrusted pod.
- **Accepted residual:** the game server's client token is a T0 root. A game server bug can speak as
  the owner to the trusted pod.
- **Replies** come back as speech (§8). Stage 1 is speak only.
- **Later:** multi-character scenes, embodiment through the game's MCP (ADR-0083's shape).

### 13. Runtimes and ADR-0002

Stage 1 runs Claude Code only. A `runtime` field and a Hermes/OpenClaw spike are later, behind the
same sidecar contract; the spike must first answer auth (subscription OAuth is Claude Code's), the
gate and sub-agents. ADR-0002 is reopened only for runtimes inside the fleet, under core.

### 14. Stages

1. **Both agents, thin.** Ops: logs, metrics, alerts, issues, spawns, cluster reads. Game host: speak
   only. Built as the slices in the appendix.
2. **The `runtime` field** and the Hermes/OpenClaw spike.
3. **Live steering** through core: pause, redirect, goals, budget.

## Alternatives considered

- **Resident definitions only, re-warmed per talk.** Rejected as the whole answer: a role loses its
  context at every teardown.
- **An agent loop inside core, or in any control plane with tools in the pod.** Rejected: an agent
  inside core's trust boundary, and Claude Code's tools run in its own process.
- **Hermes or OpenClaw as an orchestrator outside the fleet.** Rejected: two control planes.
- **One merged thread per agent.** Rejected: trusted and untrusted inputs share one context.
- **Trust per thread inside one pod.** Rejected after doubt: threads share Bash and loopback to the
  sidecar under one lease, so an untrusted thread can claim to be the admin thread.
- **A row (and lease) per thread.** Rejected: a new thread cannot attach to a running pod, and threads
  would share one `/workspace` across sessions (forbidden).
- **Cluster reads through the executor.** Rejected: the boundary would be a verb allowlist, not RBAC.
- **The fleet island as the game's admin path.** Rejected: the island reaches agents only through
  `Open`, which standing refuses, and it would put T0 in the game page's JavaScript.

## Consequences

- Four standing pods (owner to confirm) run all the time, in their own pool.
- No pod anywhere holds a credential in env; the worker always goes through the sidecar's router.
- ADR-0048 §1's gate is per Start, not per warm, for standing sessions.
- An alert can make the ops agent think and read, never act; an act from an untrusted thread is a card.
- One pod in the fleet holds a read-only kube identity.
- The game server's client token is a T0 root.
- `CLAUDE.md` and `DECISIONS.md` keep "no orchestration framework (Hermes/OpenClaw rejected)" until
  stage 2 is accepted.

## The bet

One control plane can host always-on agents. Splitting each agent by trust into two pods, with
separate leases, makes "an untrusted input never acts" a property of the lease rather than of the
prompt, and RBAC (not an allowlist) bounds what the one reading pod can see.

## Not doing

A framework over the fleet; an agent loop in core; a fleet voice runtime; a Deployment; a weekly roll;
a lease heartbeat; a shared writable directory between agents. Later: multi-character scenes, other
runtimes, narrower GitHub scope for repo pods, gVisor.

## Appendix: stage-1 build slices

Each slice is its own PR, planned against the code and doubted before the build. Tags name the
findings it must pass as acceptance tests: cycle 3 of the earlier draft (C3), and cycle 1 of this
rewrite (B, M).

1. **No credential in pod env** (fleet-wide, B3, M8): the per-pod Secret file in the sidecar only,
   the router for definition pods too, `ownerReference` on the Job; a golden test that no container's
   env holds a token or a lease.
2. **`resources` applied** from the definition to the worker container.
3. **Alertmanager onto the bridge**, with the dedicated route and the owner-only route for runaway
   and standing-pod alerts; `/webhook/alertmanager` retired (minor, M9).
4. **The `standing` lifecycle, tiers and Start** (B1, C3): the migrations of §3; Start creates both
   tier rows; `Open`, `Invoke` and routed proposals refuse a standing-only definition.
5. **Threads** (B1, M7): one SDK session per thread in the worker, the thread map and cursor in core,
   thread keys and retirement, at most 2 running threads per pod (M11).
6. **Kept up by core** (M5, M6, C3): sweep exemptions, `rewarm_after` on every terminal path, the
   crash-loop stop, the drain on a pin change, immediate teardown on revoke.
7. **Provenance and routing** (B2, C3): entries routed by tier, the cause window per thread (empty =
   untrusted), effect methods absent from the untrusted lease, card budgets per tier (M12).
8. **Capacity** (M1, C3): the standing pool, standing and spawned sessions excluded from the external
   counts, the spawn sub-pool working with `MAX_DEFINITION_SESSIONS=0`.
9. **Memory** (M2, M4): the two directories, created uid 1000 before the mount (minor), the sidecar's
   path rule, one git writer per directory, the console diff.
10. **Credentials for standing pods** (M8, C3): rotation under `lockVault`, two label annotations, the
    next label written before the mint.
11. **Reach** (B4, minor): `SpawnWorker`, `WaitForResult`, `ViewLogs` granted, `QueryMetrics`,
    `OpenIssue` scoped to `owns.repos`; the ops agent's ServiceAccount (infra-bootstrap) in its
    untrusted pod's sidecar, and the egress rule for the API server.
12. **Runaway** (M9, M10): spawn limits, the turn budget on the Europe/Paris day with the T0 bypass,
    its card and its alert formula, the per-thread wake cooldown with coalescing.
13. **The game host, thin** (B5, M3): `PostToStanding` with its authorization, scene threads,
    `CoreService.Speak` bound to the calling scene, the dispatcher dropping speech.
