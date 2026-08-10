# ADR-0034: Environment recipe system replaces the hardcoded e2e start-command switch

**Status:** Accepted
**Date:** 2026-08-10

## Context

`dream-analyst`'s e2e-preview pod failed every time: the preview loaded but
was broken. Root cause traced to
`provisioner/internal/k8s/names.go`'s `StartCmdFor`, a hardcoded per-repo Go
switch statement driving a single-container e2e-runner pod
(`provisioner/internal/k8s/pod.go`'s `CreatePod`). Two concrete bugs: Vite
never binds the host/port the pod's Service routes to (`bun run dev` reads
neither `$PORT` nor a `--host` flag), and `dream-analyst/front` is a
full-stack SvelteKit app requiring Postgres (Prisma) and Redis (`ioredis`,
per its own `compose.yml`) that the single-container pod never provisions.

A narrow fix to `StartCmdFor`'s one case would have left the structural
problem in place: no way to declare a repo's real dependencies, one
hardcoded container shape, and per-repo behavior baked into Go source
requiring a redeploy to change (`docs/adr/0028` already anticipated and
flagged this gap in its own Consequences section, leaving `StartCmdFor`
deliberately untouched at the time).

An architecture interview (`architecture-interview` skill) was run instead
of patching the symptom. It surfaced that the real need is broader than
e2e: the same repo needs different tooling for different task purposes —
`dream-analyst`'s e2e profile needs `postgres+redis+bun`, while
`agent-fleet`'s own lint/build tasks need `golangci-lint+buf`, unrelated to
browser preview. The worker/e2e pod split itself (`docs/adr/0012`/`0019`/
`0020`) was investigated and confirmed *not* the problem — a deliberate
trust-boundary decision (worker pod holds zero Kubernetes RBAC, ever) — and
is untouched by this decision.

## Decision

### Environment recipes: named profiles built from a bounded ingredient catalog

A repo declares named profiles (`"worker"`, `"e2e"`, or others like
`"lint"`) via a new `repo_profiles` table (sibling to the `repos` table
from `docs/adr/0028`), dashboard-editable, threaded core→provisioner over
the existing gRPC hub-and-spoke (`docs/adr/0020`). The provisioner still
never holds `AGENTFLEET_DB_*` fleet-coordination credentials — core
resolves a profile from Postgres and passes the *resolved ingredient list*
to the provisioner, the same way `repo_url`/`base_branch` already cross
that boundary.

Two ingredient kinds, catalog bounded to six entries for v1 — adding a new
one is a deliberate provisioner code change, never config-only:

- **Tools** (`go-toolchain`, `bun-toolchain`, `golangci-lint`, `buf`) —
  always pod-local. A terminating init container copies static binaries
  onto a shared `emptyDir`, mounted into the main container (worker or
  e2e-runner) with `PATH`/`GOROOT` pointed at it. The agent's own Bash tool
  needs these as real local binaries in its own filesystem/process
  namespace — sibling containers don't share that.
- **Services** (`postgres`, `redis`) — three selectable scope modes per
  ingredient instance in a profile:
  1. **`pod-scoped`** — native sidecar container (`restartPolicy: Always`
     init container + `StartupProbe`), the exact pattern already used for
     the worker pod's own `sidecar` container. `localhost`-only, dies with
     that one pod. For genuinely throwaway, single-pod cases.
  2. **`task-scoped`** — lives in a shared per-repo instance (Deployment +
     `Service` + PVC, auto-provisioned on first use). A per-task
     database/credentials is minted on first use and reused, over the
     network, by every other pod belonging to the *same* task — a task's
     worker pod and its concurrently-running e2e-preview pod
     (`docs/adr/0029`: a worker can call `request_e2e_env` to get a live
     preview pod alongside itself), or a later resumed worker pod. Chosen
     over pure pod-scoped sidecars because a pod-scoped DB is invisible
     outside its own pod — the worker couldn't reach the e2e pod's data (or
     vice versa) for manual tweaks/migrations, and nothing external could
     reach it either.
  3. **`repo-scoped`** — same shared-instance machinery, skips per-task
     minting: every task against the repo hits the same database (durable
     seed data reused across runs, e.g. `agent-fleet`'s own `"lint"`
     profile).

### Credential minting: provisioner-executed, synchronous, deterministic

The provisioner mints per-task/per-repo Postgres roles+databases itself,
inside the existing `CreateWorkerPod`/`CreateE2eSession` gRPC handler,
*before* creating the consuming pod — the same sequential
do-work-then-fail-loud shape already used there for git clone/worktree-add.
Name and password are **deterministic** — `task_<shortID(taskID)>` /
`repo_<repo>`, password `= hmacSHA256(adminSecret, name)` — not stored or
looked up anywhere. A second pod for the same task (or a retried request)
independently recomputes the identical credentials and re-runs the same
idempotent `CREATE ROLE`/`CREATE DATABASE` sequence, a safe no-op. This
avoids a new lookup service, a per-task Secret, or threading state back
through core, while still being race-safe under concurrent callers.

Security note: anyone able to read a shared instance's admin Secret can
recompute any of its task passwords from the task ID alone — acceptable
because that Secret's read access is gated to the provisioner's own
ServiceAccount alone, never core, never a worker pod.

### Fail-loud materialization, no new polling machinery

- `pod-scoped`: `StartupProbe` blocks the main container from starting —
  kubelet-enforced.
- `task-scoped`/`repo-scoped`: minting happens before the pod is created at
  all; a failure means `CreateWorkerPod`/`CreateE2eSession` itself errors,
  so the pod never exists — a stronger guarantee than a probe.
- Tools: a failing copy step fails the whole pod immediately
  (`RestartPolicy: Never`, already set on both pod kinds).

This directly reacts to the original bug: the old e2e pod could report
"running" while being silently broken. Nothing in this design allows that.

### Shared-instance lifecycle: uniform idle-timeout GC

Auto-provisioned on first use (no separate manual step), idle-timeout GC'd
by extending `provisioner/internal/reconcile/loop.go`'s existing tick (this
codebase already runs an autonomous provisioner-only GC pass with no core
involvement, per its `sweep` package precedent) — a `last-used-at`
annotation on the shared instance's Deployment stands in for a Postgres
timestamp column, since the provisioner has none. **Task-scoped and
repo-scoped instances share the same GC policy, no exemption for
repo-scoped** despite it holding durable data: the instance is cheap to
recreate via the same idempotent `EnsureSharedInstance` path used to
provision it the first time, so correctness rests on that re-creation path
being solid rather than on indefinite preservation.

### Profile selection: fixed convention for v1

The worker pod always resolves the profile literally named `"worker"`;
`request_e2e_env` always resolves `"e2e"` by default, agent-overridable via
a `profile` argument (reusing the existing `startCmd`-override pattern).
No per-task worker-profile picker — no case for one yet.

## Alternatives considered

- **Keep `StartCmdFor`'s hardcoded switch, just fix the two dream-analyst
  bugs** — rejected: leaves the structural gap (no way to declare real
  dependencies, redeploy required to onboard a new repo) that caused this
  bug in the first place.
- **Spec file committed in the target repo's own git tree** (CRD-like, read
  off the already-mounted worktree) — rejected: would let a task/agent
  branch influence what containers the provisioner creates on its own
  behalf, reopening the exact git-write/infra-mutation separation
  `docs/adr/0012` built the provisioner to prevent.
- **A real Kubernetes CRD + controller** — rejected for now: cluster-native
  and decoupled from any one repo's git history, but a materially bigger
  build (new CRD schema, controller/reconcile loop, RBAC surface) with no
  concrete need yet at this scale.
- **Fully generic arbitrary-container ingredient spec** (any image/command/
  env, mini-podspec) — rejected for v1, left as an explicit future door:
  bigger blast radius (config alone decides what runs with pod-level
  access), and today's real cases are fully expressible as a small known
  vocabulary.
- **Scoping the recipe system to the e2e-preview pod only** — rejected: the
  golangci-lint/buf case is a worker-pod build/lint need, not a preview
  need.
- **Best-effort/partial materialization** (only fail if the task touches
  the missing piece) — rejected: reintroduces the exact "looks fine,
  quietly isn't" failure mode that caused the original bug.
- **Per-task worker-pod profile selection** — deferred, real new scope (a
  task-level field, `CreateTask` UI change) with no concrete case yet.
- **Services always pod-scoped ephemeral sidecar** — rejected as the only
  mode: no cross-pod reachability breaks worker+e2e sharing for the same
  task and rules out external/manual DB access entirely.
- **Services always shared-instance, no pod-local option** — rejected:
  still want a pure pod-local, fully-isolated throwaway option for cases
  that genuinely don't need sharing.
- **Literally-shared single DB/credentials as the only reuse mode** (no
  per-task isolation within a shared instance) — rejected: concurrent tasks
  need isolation to work freely without corrupting each other's state; kept
  as the explicit `repo-scoped` opt-in for durable-seed-data cases instead.
- **Exempting repo-scoped instances from idle-timeout GC** — rejected:
  uniform GC is simpler than a special case, and the real requirement is a
  solid re-creation path, not indefinite preservation.
- **Consumer-side init container performs credential minting** (instead of
  the provisioner doing it server-side) — rejected: duplicates minting
  logic across the worker and e2e code paths and can't cleanly short-circuit
  "already minted by the other pod" without the deterministic-naming trick
  anyway, so centralizing in the gRPC handler is strictly simpler.

## Consequences

- `provisioner/internal/k8s/names.go`'s `StartCmdFor` switch and the
  `E2E_START_CMD_<REPO>` env-override are deleted once the rollout
  completes (kept temporarily as a rolling-deploy fallback only).
- The provisioner gains a new, distinct responsibility: administering the
  shared per-repo Postgres/Redis instances it creates itself (its own
  throwaway infrastructure) — including minting/dropping per-task databases
  and credentials inside them. This is separate from, and does not reopen,
  `docs/adr/0020` point 1's fleet-coordination-DB boundary.
- New RBAC verbs on the provisioner's `Role`: `apps/deployments` and
  core `secrets`/`persistentvolumeclaims` (create/get/list/watch/delete) —
  needed to materialize and garbage-collect shared instances.
- `repo_profiles`/`repo_profile_tools`/`repo_profile_services` become new
  dashboard-editable config surfaces, following `docs/adr/0028`'s exact CRUD
  RPC precedent.
- Extending the catalog with a new ingredient (not yet one of the six known
  keys) requires a provisioner code change by design — config alone cannot
  introduce a new container type.
- Who is authorized to create or edit a repo's environment recipe (human
  vs. agent) is explicitly deferred — human-only today, no new
  authorization layer built as part of this decision.

## Out of scope / deferred

Recipe-edit authorization policy; the fully-generic arbitrary-container
ingredient spec; per-task worker-pod profile choice; a
`ListIngredientCatalog` validation RPC (dashboard-side ingredient-name
validation currently only happens at pod-materialization time — add this
if config typos become a real operational problem); exact resource
requests/limits on catalog containers (tune once running on the real
cluster).
