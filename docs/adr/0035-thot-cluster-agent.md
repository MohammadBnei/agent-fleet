# ADR-0035: `thot`, a standing cluster agent with its own Kubernetes RBAC

**Status:** Accepted
**Date:** 2026-08-10

## Context

The fleet has no standing way to inspect or fix live `ukubi-cluster` state,
and no way for a worker mid-task to ask "why did this break" and get a real
answer. Today that gap is filled only by a human manually SSHing into
`k9s-dashboard` (infra-bootstrap's `/k8s-ops` skill, a cluster-admin
kubeconfig, session-scoped human authorization) — there's no automated,
fleet-integrated, or continuously-alert-driven path.

This was designed through an architecture interview and an adversarial
(doubt-driven) review before any code was written, same discipline as
ADR-0012. The review was grounded in this repo's and infra-bootstrap's
actual ADRs, RBAC manifests, and secrets docs — not just the proposal
text — and it caught a factual error in the interview's own reasoning
(see below) plus three real open design forks, all resolved with the user
before this ADR was written.

**The corrected rationale, stated plainly:** this decision deliberately
reopens ADR-0012's rejected trust boundary — "the thing holding
write-in-git trust never also holds infra-mutation trust." It is *not*
reopened because `canUseTool` (ADR-0029) resolved the concern ADR-0012
raised — it didn't. ADR-0005's human-approval gate already existed when
ADR-0012 was written, and ADR-0012's own rejection text cites ADR-0005 by
name; trust-domain separation was rejected as a *distinct* concern from
"is each call checked by a human," and per-call confirmation doesn't
answer it. This ADR reopens that boundary as a **conscious, accepted
trade-off**, mitigated but not eliminated by live per-call human
confirmation — not as a resolved one. The capability itself isn't new
either: `/k8s-ops` already reaches a broader, cluster-admin-scoped path via
SSH. `thot` automates and fleet-integrates that same class of access
(continuous alerts, scheduled audits, synchronous answers for other
workers) rather than introducing access that didn't exist before.

## Decision

Add `thot`, a new persistent, GitOps-deployed cluster agent with standing
Kubernetes RBAC over `ukubi-cluster`. It is reachable directly and in real
time by worker sidecars, cluster alerts, and humans via its own
protobuf/gRPC service — a stated, explicit exception to the fleet's
hub-and-spoke topology (ADR-0020 pt. 4/6), scoped to the *live call path
only*. All of `thot`'s findings/actions still persist through `core` (the
fleet's sole Postgres-credential holder, unchanged by this decision), and
all durable cluster changes still go through the existing feature-branch +
PR flow, no auto-merge — the same as every other fleet output.

Quality-attribute priority: reachability/immediacy over strict
hub-and-spoke purity. Safety comes from live per-call human confirmation,
always required — no timeout, no unattended fallback, even for
alert-triggered incidents. Slower incident response is an accepted cost of
never inferring approval from silence, consistent with the fleet's
existing structured-approval rule (`docs/DECISIONS.md` §2).

### Constraints

- `core` remains the sole Postgres-credential holder (ADR-0020 pt. 1,
  unchanged). Any direct sidecar↔`thot` Q&A must be double-written into
  the requesting task's `knowledge_journal`/`transcript` via `core` —
  answered-and-forgotten is not acceptable, it breaks the fleet's existing
  audit trail.
- RBAC exclusions, architectural and non-negotiable, not just an
  implementation detail: `thot` must never be granted
  `rbac.authorization.k8s.io` verbs (no self-escalation path), never
  blanket `secrets` read, never node-level verbs (that's `node-drainer`'s
  territory in infra-bootstrap). Kubernetes-API boundary only — no
  Proxmox/VM layer (terraform, ansible-playbook, kubespray, pigsty
  install/scale stay human-only, unchanged).
- Mutation exclusion list: `thot` must never target `core`'s or
  `provisioner`'s own pods, or any pod holding an active git-worktree
  lock — echoes the ADR-0023 incident (worktree deletion as an unintended
  side effect of a "safe-looking" teardown).
- `thot`'s Discord channel is strictly notify-only. It is never an
  implicit approval path — approval is always the structured
  `RespondToPermission` call via `canUseTool`, never inferred from a
  Discord reply (same reasoning ADR-0029 already applied to excluding
  `permission_request`/`permission_response` from the per-task Discord
  relay).
- `thot`'s own `ServiceAccount`/`Role`/`ClusterRole` is a standing,
  human-reviewed manifest in infra-bootstrap's `gitops/` (the same pattern
  as `actions-runner`, `node-drainer`, and ADR-0012's own
  `e2e-provisioner`) — **never** dynamically created by agent-fleet's
  `provisioner`. Giving `provisioner` the power to mint RBAC objects for
  another pod is a privilege-escalation primitive no fleet component holds
  today and shouldn't gain; `core`/the dashboard talk to `thot` as a peer
  over its own gRPC service instead.

### Capabilities

1. Live, direct, reversible cluster mutation, gated per-call by
   `canUseTool` (restart a rollout, delete a crashlooping pod — subject to
   the mutation exclusion list above).
2. Durable fixes/features (a manifest, an RBAC change, a new resource) go
   through the standard git + PR flow against infra-bootstrap, same as any
   other fleet output.
3. Called directly (own protobuf/gRPC service, not proxied through `core`)
   by worker sidecars mid-task for synchronous Q&A ("why did this break")
   — the explicit hub-and-spoke exception, scoped to this live call path
   only; every exchange double-writes into `core`'s journal, correlated to
   the asking task.
4. Triggered by cluster alerts (a new Alertmanager receiver — cross-repo
   integration owned by infra-bootstrap's `gitops/`) and by
   scheduled/periodic audits wired via the fleet dashboard. Both are new
   trigger types; neither fits `core`'s existing 30s-tick/nudge dispatch
   loop, which only reacts to task-creation/status-change/crash nudges
   today — a new scheduling mechanism is needed, left to implementation.
5. Proactive Discord channel for urgent findings, distinct from the
   existing per-task thread relay — strictly notify-only per the
   constraint above.
6. Standing identity/continuity as an always-on GitOps-deployed
   `Deployment`, not a `provisioner`-spawned ephemeral pod — supervised by
   normal Kubernetes liveness/readiness and ArgoCD sync-health, not
   `core`'s idle-timeout/session-warm machinery. That machinery is generic
   across `SessionKind`s (`WORKER`/`E2E`) today; `thot` deliberately isn't
   one of them.

## Alternatives considered

- **No RBAC for `thot`, `provisioner` proxies cluster calls** — rejected:
  conflates `provisioner`'s narrow fleet-self-management purpose with
  open-ended cluster-wide ops under one identity.
- **Route `thot`'s live interactions through `core`** (strict
  hub-and-spoke) — rejected: a central broker adds latency/queuing
  friction for a component whose entire point is real-time reachability
  from anywhere in the fleet.
- **`provisioner` dynamically creates `thot`'s pod + `ServiceAccount` +
  `Role`** — rejected after adversarial review: hands `provisioner`
  RBAC-object-creation power, a privilege-escalation primitive no fleet
  component holds today.
- **Ephemeral, single-shot dispatch** (same model as today's
  `WORKER`/`E2E` sessions) — rejected: loses the continuity/identity the
  alert and scheduled-audit use cases need, and doesn't naturally support
  standing Discord/UI wiring.
- **Read-only diagnostic role, mutation always via PR** — rejected: no
  path for cheap, reversible live fixes without a full PR cycle for
  something trivial (e.g. a stuck pod).
- **Reuse the existing `/k8s-ops` SSH → `k9s-dashboard` cluster-admin path
  as-is, no new component** — rejected: that path is manual and
  session-scoped by design; it can't answer a worker's question mid-task
  or react to an alert on its own.

## Consequences

- Deliberately, consciously reopens ADR-0012's trust-domain-separation
  principle as an *accepted* risk, not a resolved one — write-in-git trust
  and infra-mutation trust now coexist in one component, mitigated but not
  eliminated by live per-call confirmation.
- A second, independently-deployed RBAC holder now exists in the fleet's
  orbit, alongside (not replacing) `provisioner` — `provisioner`'s own
  RBAC stays exactly as narrow as it is today.
- The sidecar's outbound trust surface doubles (one channel to `core`, one
  to `thot`) — needs its own auth model of its own; the provisioner's
  network-reachability-only precedent (ADR-0012's Consequences) doesn't
  scale to a component with open-ended cluster-mutation power and is
  explicitly *not* reused as-is.
- New cross-repo build surface: infra-bootstrap gains a new
  `gitops/platform/thot/` standalone Application (modeled on
  `e2e-provisioner`/`actions-runner`) plus a new Alertmanager receiver
  config; agent-fleet gains a new protobuf service, a new MCP tool for the
  sidecar→`thot` call, a new dashboard-driven audit-scheduling mechanism,
  and a new standing Discord bot identity.
- `docs/DECISIONS.md`'s hub-and-spoke bullet now needs a named exception
  rather than reading as absolute.

## Out of scope / deferred

VM/Proxmox-layer operations (unchanged, stays human-only). Exact RBAC
verb/namespace enumeration (bounded by the exclusions stated above). Exact
wire shape of the sidecar↔`thot` protobuf service and its auth model.
Exact Alertmanager receiver wiring and the audit-scheduling mechanism. All
of this is real design/build work for a follow-up `/fleet-feature` pass,
not this ADR.

## Reversibility

One-way door — new standing credential, new cross-repo component, and a
deliberate, consciously-accepted exception to two previously locked
invariants (ADR-0012's RBAC boundary, ADR-0020's hub-and-spoke topology).
