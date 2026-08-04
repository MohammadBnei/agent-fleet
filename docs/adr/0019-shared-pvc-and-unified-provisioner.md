# ADR-0019: Shared worktree PVC and a unified provisioner replace per-repo persistent workers

**Status:** Accepted
**Date:** 2026-08-04

## Context

Today, one persistent worker pod per target repo (`dream-analyst-worker`,
`vos-monolith-worker`), `replicaCount: 1`, each with its own dedicated RWX
workspace PVC holding that repo's clone plus every task worktree ever
created against it (ADR-0003). This caps concurrency at exactly one
in-flight task per repo, and onboarding a new repo means a whole new
Deployment + PVC, not just a queue entry (`/fleet-ops`). Note this is a
different PVC from ADR-0010's `agent-fleet-shared-pvc` (`/mnt/fleet-shared`,
skills/journal-mirror/MCP configs) — this decision concerns the per-repo
*workspace* PVCs only.

The real, stated goal is ~5 concurrent tasks across repos — the actual
human-followable ceiling for one solo operator, not a technical limit
(cluster hardware is being upgraded, so capacity isn't the constraint
here). This is also deliberately step one of a longer roadmap — cross-repo
agent-to-agent knowledge sharing, and eventually other autonomous agent
types (e.g. one with `infra-bootstrap/gitops` access) — that this decision
does not build, but must not foreclose.

Reached through a live architecture interview; the Alternatives below
reflect what was actually raised and rejected in that interview, not
reconstructed after the fact.

## Decision

1. **One shared RWX PVC replaces the two per-repo workspace PVCs.** It
   holds every repo's clone (fetched/updated in place, never re-cloned per
   task) plus every task's worktree, keyed by the already-globally-unique
   `task.id`, not nested per repo. Exact path layout is still flexible
   implementation detail, but ownership is not (see next point) — this was
   originally deferred wholesale, then tightened after ADR-0020's
   doubt-driven review flagged the ownership question as a correctness
   gap (a shared-clone data race), not a layout bikeshed.

2. **The provisioner owns the entire git lifecycle on the shared PVC —
   clone, fetch, and worktree add/remove — worker pods never do.** Today's
   `worker/src/git.ts` (`createWorktree`/`removeWorktree`, clone-if-missing)
   moves to the provisioner. Concretely: before calling `CreateWorkerPod`
   for a never-before-seen repo, the provisioner clones it once; before
   every dispatch it fetches; it creates the task's worktree + branch
   (`git worktree add -b agent/<taskId> ...`) *before* the pod exists, and
   removes that worktree after the pod's task reaches a terminal state —
   the same lifecycle position teardown already occupies for e2e pods.
   Worker pods are handed an already-prepared worktree path and never run
   `git clone`/`git worktree add` themselves; the worker's `git.ts`
   responsibilities shrink to operating *inside* that path only (branch is
   already checked out, base commit already there).

   This isn't just tidiness: since the provisioner is the *only* process
   that ever mutates the shared base clone or its worktree metadata, and
   it's a single process, every such mutation is naturally serialized in
   memory (e.g. a per-repo mutex around clone/fetch) — no PVC-level file
   lock is needed at all. The alternative danger this avoids: once
   concurrency is fleet-wide instead of one-pod-per-repo (the entire point
   of this ADR), two independently-dispatched worker pods racing a `git
   clone`/`git worktree add` against the same not-yet-existing path would
   have been a real data-race risk with no owner to prevent it.

3. **`e2e-provisioner` is renamed and evolved into a single provisioner**
   owning *all* cluster-pod creation in the fleet — its existing e2e-preview
   pod lifecycle (unchanged: RBAC, `NetworkPolicy`, path-based routing, GPU
   deferral, all per ADR-0012) plus a new worker-pod lifecycle. One
   namespaced `Role` for both (create/delete Pod, Service, IngressRoute,
   Middleware) — worker pods need only the Pod verbs, a strict subset of
   what already exists. `fleet-core` stays zero-cluster-RBAC and remains a
   gRPC client of the provisioner (the same relationship it already has for
   `/e2e-kill`), extended to relay live worker-provisioning status (pod
   creating → online → task handed off) into the dashboard.

4. ~~The provisioner claims the task itself (`SELECT ... FOR UPDATE SKIP
   LOCKED` against `tasks`) as part of deciding whether to spawn a pod.~~
   **Superseded by ADR-0020 point 2**, which was caught contradicting
   point 1's "no other component ever holds `AGENTFLEET_DB_*`
   credentials" during that ADR's own doubt-driven review — this ADR
   originally proposed the provisioner as an autonomous poller, but
   fleet-core claims and *commands* the provisioner instead (matching how
   `e2e-provisioner` already behaves today: reactive, told what to do, not
   self-directed). Worker pods are still single-shot either way — handed
   one `task_id`/`repo` at creation, run exactly that task, then exit —
   this part of the original point stands regardless of who does the
   claiming.

5. ~~Concurrency is capped fleet-wide at the provisioner.~~ **Also
   superseded by ADR-0020 point 3** — fleet-core enforces the cap as part
   of the same dispatch decision, not the provisioner. The cap itself
   (~5, fleet-wide not per-repo) stands.

6. **The provisioner also seeds and maintains a shared skills/tools
   directory and a global `CLAUDE.md` on the same PVC**, replacing today's
   worker-image-baked copy (`PLANNING_SKILLS_PLUGIN_PATH =
   "/app/worker/skills/agent-fleet-planning"`, an in-image absolute path
   chosen specifically because there was "no deployment variance to
   configure around, only one image" — that premise no longer holds once
   worker pods are ephemeral and generic across repos). Worker pods read
   this PVC-resident config at startup instead of carrying their own copy.
   This follows the same PVC-as-Claude-Code's-own-state principle the
   worktrees/sessions already follow (skills/tools/`CLAUDE.md` are
   Claude Code's own data, not fleet coordination data — Postgres stays
   for the latter). It also doubles as the first concrete substrate for
   the deferred cross-repo knowledge-sharing goal: one shared,
   provisioner-maintained corpus every worker pod can read regardless of
   which repo it's processing, with no new coordination service.

## Alternatives considered

- **Raise `replicaCount` on the existing per-repo Deployments.** Rejected:
  keeps the per-repo Deployment/PVC coupling (a new repo still means a new
  Deployment), and `CLAUDE_CONFIG_DIR` (ADR-0016) is shared per *worker
  pod* today, not per task — N replicas of one repo's worker would race on
  the same Claude Code session/credential-cache directory. Namespacing it
  per-task would be required either way, at which point the ephemeral
  model is barely more work and removes the per-repo ceiling entirely
  instead of only raising it.
- **Fold worker provisioning into `fleet-core` directly.** Rejected:
  reverses fleet-core's stated zero-cluster-RBAC design
  (`docs/ARCHITECTURE.md` §5) — the same invariant that originally
  justified splitting `e2e-provisioner` out as its own service in the
  first place.
- **A second, separate `worker-provisioner` service alongside
  `e2e-provisioner`** (the initial proposal mid-interview). Rejected on
  reflection: both would need an identical namespaced `Role` and an
  identical reconcile-loop shape, just watching two different tables
  (`e2e_sessions` vs. `tasks`). The provisioner's own RBAC doesn't scale
  with what a *spawned pod* can later do — a worker pod's elevated
  credentials (`GH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`) are injected into
  that pod via Infisical at creation time, never held by the provisioner
  itself. A second `Role` would duplicate without adding real isolation.
- **Spawned worker pods self-claim via their own poll loop after
  starting** (ephemeral, but otherwise like today's claim logic).
  Rejected: splits "decide whether to spawn" and "decide what to claim"
  into two mechanisms with two race windows instead of one. (This
  reasoning originally argued for the provisioner doing both in one poll
  loop; ADR-0020 moved both to fleet-core instead — see point 4/5 above —
  but the core objection to splitting the two decisions across mechanisms
  still holds either way.)
- **A distributed lock on the shared PVC** (e.g. a lockfile, `flock`) to
  serialize concurrent `git clone`/`worktree add` calls from multiple
  worker pods. Rejected in favor of point 2 above: giving the provisioner
  sole ownership of these operations makes a distributed lock unnecessary
  — a single Go process serializing in-memory is strictly simpler than
  coordinating a filesystem lock across pods, and was only ever needed
  because an earlier version of this ADR had worker pods doing their own
  clone/worktree-add.
- **This decision knowingly reopens something ADR-0012 explicitly
  rejected**: "Ephemeral per-task Kubernetes Job spun directly by the
  worker — rejected: ... reintroduces the per-task-pod-lifecycle pattern
  ADR-0003 already moved away from for the worker itself." What changed:
  ADR-0003 moved away from per-task pods specifically for *cold-start
  cost* (a full `git clone` + MCP boot on every task). This decision
  removes that cost by making the git clone PVC-resident and persistent
  across pods — never re-fetched per task — instead of by keeping the pod
  itself persistent. The concurrency ceiling motivating this decision
  wasn't a stated problem when ADR-0003/0012 were written. The RBAC
  boundary ADR-0012 was actually protecting (workers never spawn pods,
  only the provisioner does) is unchanged and preserved here.
- **Keep skills/tools/`CLAUDE.md` baked into the worker image (status
  quo).** Rejected: every skill/tool change requires a new image build and
  redeploy, and a single fixed-at-build config can't support the
  cross-repo knowledge-sharing goal (nothing to share into, since each
  pod only ever sees its own image's copy) or, later, a different tool set
  for a different agent type. Injecting config over the network per-pod
  at spawn time was also considered and rejected in favor of the PVC:
  it would make the provisioner a runtime dependency of every session
  start (another live-call failure mode, the same category already
  rejected earlier in this ADR for diff/branch/timing data) where a
  PVC read has none.

## Consequences

- **ADR-0003 is superseded by this decision.**
- `e2e-provisioner` is renamed and gains a second watch-loop/responsibility
  — ADR-0012's own decision (RBAC boundary, `NetworkPolicy`, path routing,
  GPU deferral) stays intact and unchanged; only its scope of *what it's
  allowed to spawn* grows to include worker pods.
- The worker image's entrypoint changes from an infinite poll loop to
  single-shot (run one task, exit) — a real rewrite of the worker's main
  loop, not a config change.
- **ADR-0016 needs a follow-up amendment.** It assumed a persistent pod
  that itself notices and retries a stale claim; under single-shot pods, a
  crashed pod can't retry itself — the provisioner's reconcile loop becomes
  the thing that notices a stale/orphaned claim and respawns.
- The two existing per-repo RWX PVCs are replaced by one shared PVC — a
  real migration (new PVC, repo clones seeded onto it, old PVCs deleted),
  same caliber of change as ADR-0012's RWO→RWX migration, not a quiet
  reconcile.
- The worker image no longer bakes in `PLANNING_SKILLS_PLUGIN_PATH` —
  `worker/src/planning.ts` points at a PVC-mounted path instead, and the
  provisioner needs a sync mechanism to keep that PVC copy current from
  this repo's own `worker/skills/agent-fleet-planning/` source (exact sync
  mechanism — on provisioner startup, on a timer, or on deploy — is still
  open; the *ownership* is not, see point 2 above).
- `worker/src/git.ts`'s `createWorktree`/`removeWorktree`/clone-if-missing
  logic moves to the provisioner (point 2) — a real code migration from TS
  to the provisioner's Go codebase, not a config change.
- **Deferred to follow-up work**, explicitly out of scope here: exact
  shared-PVC path layout (now that ownership is resolved, this is genuinely
  just naming); cross-repo knowledge-sharing between agents; any future
  autonomous infra-agent work against `infra-bootstrap/gitops`
  (`infra-bootstrap/CLAUDE.md` and this repo's `docs/DECISIONS.md` both
  currently block exactly that — this decision doesn't change either
  boundary).
- **Immediate next documentation checkpoint**: an observability-to-dashboard
  architecture overview — how this topology's live state (pod
  creating/online/task handed off, per-task status) surfaces through
  fleet-core into the dashboard.
