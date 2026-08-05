# ADR-0022: Worker sessions run as `batch/v1.Job`, not hand-rolled bare Pods

**Status:** Accepted
**Date:** 2026-08-05

## Context

The provisioner (ADR-0019) creates worker pods directly via `client-go`
and, until now, hand-rolled their entire lifecycle: create a bare `Pod`,
poll it every 10s from `reconcile/loop.go` for a terminal phase, delete it
once found. Retry/backoff/GC — the exact semantics Kubernetes' own
`batch/v1.Job` resource already provides — were reimplemented by hand,
duplicated across the two pod kinds this package creates (worker pods and
on-demand e2e-preview pods).

Reliability audit finding #11 (`docs/reliability-findings.md`) raised the
question of whether to go further and adopt a full CRD + operator
(`controller-runtime`) instead — motivated by a speculative future
"on-demand infra-DevOps agent" feature that would want deep,
`kubectl`-native cluster visibility. That speculation turned out, on
inspection, to be a separate concern (what RBAC/tooling *that* agent's own
worker pod would need for live diagnostics) — orthogonal to how this
fleet's own ephemeral pods get created, and explicitly out of scope here.

## Decision

Adopt `batch/v1.Job` for **worker pods only**, replacing the hand-rolled
bare-`Pod` create/poll/GC path. No CRD, no `controller-runtime`, no new
deployable — this is a swap of the underlying Kubernetes primitive, not a
new architecture.

- **`BackoffLimit: 0`** — deliberately no k8s-level pod retry. `core`'s
  own `ClaimNextTask` heartbeat/reclaim (ADR-0020 point 2/3) stays the
  sole retry mechanism; a k8s-level retry running independently would
  silently double up against it, and would mean the provisioner deciding
  on its own to re-run a pod — exactly the autonomy ADR-0020 point 2
  forbids it.
- **`TTLSecondsAfterFinished: 300`** — the Job (and its owned Pod) is
  garbage-collected 5 minutes after finishing, long enough to `kubectl
  logs` a crashed worker before it's gone, short enough not to
  accumulate. This is what most of `reconcile/loop.go`'s old hand-rolled
  GC now defers to.
- **`provisioner/internal/reconcile/loop.go`'s reconcile loop stays a
  ticker-poll** (`RECONCILE_INTERVAL_MS`, still 10s by default) — not
  converted to a `client-go` informer/watch, despite that being floated
  as a possible follow-up when this finding was first drafted. `Job`'s own
  TTL already absorbs most of the GC burden this loop used to carry by
  hand; what's left (detecting a terminal phase to report a crash, see
  ADR-0024) doesn't need sub-second latency, and every other loop in this
  codebase (`core`'s dispatch loop, its transcript relay) is already a
  plain ticker-poll. Documenting what actually shipped, not the
  pre-implementation draft.
- The loop gained a narrow `EventReporter` interface
  (`ReportEvent(ctx, event *PodEvent)`) — on detecting a `Job` that
  reached `Failed` phase, it reports a crash event to `core` *before*
  GC'ing the Job. This is the wiring ADR-0024's fast-path crash detection
  is built on.
- A `batch/jobs` RBAC grant (`create`/`get`/`list`/`watch`/`delete`) was
  added to the provisioner's namespaced `Role` (`k8s/provisioner/
  role.yaml`) — still never a `ClusterRole`, matching ADR-0019's existing
  scoping. Notably, this grant was **missing** from the first
  implementation pass and only caught by smoke-testing a real worker Job
  against an actual `kind` cluster as the provisioner's own
  `ServiceAccount` — the fake-`client-go`-clientset unit tests that cover
  the rest of this package don't enforce RBAC at all, so a gap like this
  is invisible to them by construction. Worth remembering the next time
  a new resource type is added: RBAC needs a real-cluster check, not just
  a fake-clientset one.
- **e2e-preview pods deliberately stay bare Pods** (`provisioner/
  internal/k8s/pod.go`'s `CreatePod`, unchanged). They're long-running and
  interactive — a developer keeps one open, code-server and Playwright
  attached, until explicitly killed via `KillE2eSession` — not
  run-to-completion work. `Job`'s retry/backoff/TTL semantics are built
  for a workload expected to finish; forcing that shape onto a pod with
  no expected completion would add an extra Kubernetes object with none
  of `Job`'s actual benefits.

## Alternatives considered

- **Client-go informers/watches on bare Pods, no `Job`.** Event-driven
  instead of polling, but still hand-rolls the retry/backoff/GC semantics
  `Job` already provides for free. Strictly worse than adopting `Job`
  outright, not wrong on its own terms.
- **A CRD as a passive mailbox, with a thin controller doing only
  spec→pod mechanics (no autonomous convergence).** Rejected: no consumer
  actually needs `kubectl`-native visibility into worker sessions today.
  The dashboard already gets pod/worktree state through `core`'s own RPC
  surface (`ListWorktrees`, ADR-0023), and `core` deliberately holds no
  cluster RBAC to read custom resources directly anyway (ADR-0020 point
  1) — a CR's `.status` would still need relaying through that same RPC
  hop, not bypass it.
- **A full autonomous CRD + operator.** Rejected: the operational cost —
  a CRD schema is a one-way door once anything depends on its shape,
  leader election, a new cluster-scoped RBAC surface — isn't justified at
  this scale (concurrency cap ~5, one provisioner replica), and an
  operator's entire point is autonomous convergence, which risks blurring
  the locked invariant that the provisioner never decides pod lifecycle
  on its own (ADR-0020 point 2). The speculated future driver (an
  alert-triggered infra-DevOps agent) turned out to be a separate
  concern on inspection — what tooling *that agent's own pod* needs, not
  how this fleet's ephemeral pods get created.

## Consequences

- Worker and e2e-preview pods are now templated through genuinely
  different code paths in `provisioner/internal/k8s/pod.go` (`Job` vs.
  bare `Pod`) — an intentional divergence, not leftover duplication; they
  have different lifecycle shapes and shouldn't share a code path just
  because they used to.
- `provisioner/internal/reconcile/loop.go` keeps owning pod lifecycle (no
  new component to build, deploy, or monitor) while getting `Job`'s
  retry/backoff/GC semantics for free instead of hand-rolling them; it
  doesn't eliminate the reconcile-loop→`core` reporting hop (ADR-0024
  covers that), just removes the hand-rolled mechanics underneath it.
- **Reversibility: a two-way door.** `Job` is a built-in Kubernetes
  resource — nothing to migrate away from if a CRD/operator becomes
  justified later (e.g. a genuine future need for externally-authored
  declarative desired state, or real `kubectl`-native visibility). This
  decision doesn't foreclose that path, it just doesn't pre-build it
  speculatively.
