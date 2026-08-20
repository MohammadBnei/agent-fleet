# ADR-0047 — Prometheus metrics scoped to the two hubs, scraped by ServiceMonitor

- **Status:** Accepted — amended by [0059](0059-metrics-off-the-gated-port.md),
  which moves core's `/metrics` off 8080 onto its own port. The metrics, their
  names and their labels are untouched
- **Date:** 2026-08-14
- **Relates to:** [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core's zero-RBAC rule,
  which shapes how the topology view gets its data),
  [ADR-0019](0019-shared-pvc-and-unified-provisioner.md) / [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md)
  (worker pods are single-shot Jobs — the reason their telemetry stays in
  logs), [ADR-0042](0042-console-rewrite.md) (the console this adds a view
  to), [ADR-0046](0046-context-budget.md) (the SDK token fields this
  surfaces, already logged by `session.ts`)

## Context

The fleet had no time-series metrics at all. Everything observable went
through Loki: every component logs JSON/slog, Alloy ships it, and the
Grafana dashboard in `k8s/core.yaml` was seven panels of cAdvisor,
kube-state-metrics, and LogQL. That answers "what happened" well and
"how much, over time" badly — there was no expression that could alert on
*"tasks have been sitting pending for thirty minutes"*, because a pending
task has no pod for kube-state-metrics to count and writes no log line for
Loki to match.

A first attempt (PR #154) added `/metrics` endpoints to all four
components. It did not work, in four independent ways, and each failure is
worth recording because none of them would show up as a red test:

1. **It did not compile.** `prometheus/client_golang` was never added to any
   `go.mod`. Caught by CI, but only after review had already looked at it.
2. **Nothing was instrumented.** Twenty metrics were declared with
   `promauto` and none were ever incremented — the only references anywhere
   were blank imports. `/metrics` would have served Go runtime defaults and
   nothing else, and a metric that renders as *absent* reads identically to
   a feature that is broken.
3. **The scrape config scraped nothing.** It used `prometheus.io/scrape`
   pod annotations. This cluster runs kube-prometheus-stack — the Prometheus
   Operator — and `infra-bootstrap`'s
   `gitops/platform/values/prometheus/values.yaml` declares no
   `additionalScrapeConfigs`, so there is no `kubernetes-pods` job for an
   annotation to drive. The annotations were inert, and worse, they *looked*
   like scraping was configured.
4. **The worker exporter was dead and redundant.** It bound `:9092` while
   the annotation advertised `:9091`, and it re-derived numbers
   `worker/src/session.ts` already logs.

The common thread is the one `CLAUDE.md` already names: a bound port is not
a reachable service is not a working capability. Every layer here passed the
one below it.

## Decision

### 1. Metrics come from `core` and the `provisioner` only

Not the sidecar, not the worker.

Worker and sidecar pods are single-shot Jobs. A session that runs three
minutes has six chances to be scraped at a 30s interval, and a short one may
have none — so a counter on them samples a *random subset* of sessions,
which is worse than no number because it looks authoritative. Their metrics
were also labelled by `task_id`, which is unbounded cardinality by
construction.

The SDK's turns/cost/token counts stay where `session.ts` already puts them:
structured log fields, shipped to Loki, which keeps every line regardless of
how briefly the pod lived. The Grafana dashboard queries them with LogQL
`unwrap`. Same data, on the storage engine whose retention model actually
matches an ephemeral producer.

### 2. Eight metrics, each one incremented

The bar for inclusion: **not already answerable from Loki or
kube-state-metrics.** That excludes pod counts, phases, restarts, CPU,
memory (kube-state-metrics/cAdvisor), and per-RPC detail (already
access-logged).

| Component | Metric | Where it is incremented |
|---|---|---|
| core | `agentfleet_grpc_requests_total{surface,method,status}` | one interceptor per surface |
| core | `agentfleet_grpc_duration_seconds{surface,method}` | same interceptors |
| core | `agentfleet_tasks_current{repo,status}` | dispatch tick |
| core | `agentfleet_live_pods` | dispatch tick |
| core | `agentfleet_max_in_flight` | dispatch tick |
| provisioner | `agentfleet_provisioner_grpc_requests_total{method,status}` | interceptor |
| provisioner | `agentfleet_provisioner_grpc_duration_seconds{method}` | interceptor |
| provisioner | `agentfleet_provisioner_pods_{created,deleted}_total{type}` | `k8s/pod.go` |
| provisioner | `agentfleet_provisioner_git_operation_duration_seconds{operation}` | `git.Manager.run` |

Three choices inside that worth naming:

- **Interceptors, not per-handler calls.** Both surfaces already have an
  access-log interceptor; the metrics one chains alongside. Every current
  and future method is covered with no per-handler edit to forget.
- **`git.Manager.run`, not its callers.** Every git command in the package
  routes through that one function, so one timer covers clone/fetch/worktree
  add and remove. Custom buckets out to 300s: a cold clone runs well past
  `DefBuckets`' 10s ceiling, and that tail is the entire reason to measure
  it.
- **`live_pods` *and* `max_in_flight`, not a "dispatch outcome" counter.**
  Either gauge alone is ambiguous. Together they separate a saturated fleet
  (`live == max`, pending > 0) from a stalled dispatcher (headroom to spare,
  pending > 0) — and unlike a claim-outcome counter, they need no change to
  `ClaimNextTask`'s single-query design.

No `task_id` label anywhere. Per-task detail is what the transcript and Loki
are for.

### 3. ServiceMonitors, not annotations

`serviceMonitorSelectorNilUsesHelmValues: false` in the prometheus values
means Prometheus watches ServiceMonitors cluster-wide, so no `release:`
label is needed. `core` gets one through `common-app-chart`'s
`extraManifests`; the provisioner gets a standalone one.

The provisioner's `Service` also had to *gain labels* — it had none at all,
and a ServiceMonitor selecting an unlabelled Service matches nothing and
emits no `up` series, which is indistinguishable from a healthy scrape until
someone goes looking for the metrics. This was verified by rendering the
chart and diffing the ServiceMonitor's selector against the Service the same
render produces, rather than by inspection.

### 4. The dashboard proxies Prometheus; it does not gain RBAC

The console gets a sixth view: a live fleet topology plus a PromQL explorer.

`QueryMetrics` proxies PromQL through `core` because Prometheus has **no
IngressRoute** in this cluster — only Grafana and Alertmanager do — so a
browser has no route to it. Giving it one would expose every cluster metric
to anything that resolves the hostname; `core` is already behind
basic-admin-auth and the dashboard CSRF header. The query is bounded
server-side (non-empty, range ≤ retention, parseable step): authenticated is
not unbounded.

`GetFleetTopology` adds **no cluster access**. Under ADR-0020 point 1, `core`
holds zero RBAC, and a topology picture is not a reason to grant it any.
Every worker pod's existence is already mirrored into `tasks.pod_phase` by
`ReportPodEvents` — the same signal the task list renders — so the cells come
from Postgres and the rates come from PromQL.

Prometheus being unreachable **degrades** that response rather than failing
it: the cells still return, with `metrics_error` set and surfaced in the UI.
Returning zeroed rates silently would render a busy fleet as an idle one.

Prometheus' result envelope travels as raw JSON rather than being re-modelled
in proto. The frontend already has to handle vector/matrix/scalar, and a
parallel schema would only be a second thing to keep in sync with an API we
do not own.

### 5. Grafana keeps being the place for time-series work

One new row on the *existing* dashboard, not two new dashboards. The
Observability page is deliberately thin next to it and links to it; what it
adds that Grafana cannot is that a cell maps back to the session it is
running, so a misbehaving pod is one click from its transcript.

## Consequences

- Two components expose `/metrics`; two deliberately do not. Adding an
  exporter to the sidecar or worker means reopening §1 — the objection is
  the pod lifetime and the label cardinality, and neither changes by
  wanting the number more.
- `core` now depends on Prometheus being reachable for *rates*, and on
  nothing for the rest. That asymmetry is intentional and visible in the UI.
- The Observability view renders inline SVG with a hand-written tiered
  layout — no `d3`/`cytoscape`. The graph is two fixed hubs and at most
  `MAX_IN_FLIGHT_TASKS` worker cells, a bounded shape whose positions are a
  loop, in a bundle compiled into `core`'s binary. A layout engine earns its
  weight when positions cannot be computed directly; revisit if the fleet
  ever grows a topology that is not tiers.
- **Verification bar for this change**, given how the first attempt failed:
  a metric must be observed with a *non-default value* on a running
  `/metrics` endpoint, not merely present; the ServiceMonitor selector must
  be matched against a rendered Service, not read; and the Observability
  page must be loaded in a real browser showing real cells, not asserted to
  have built. All three were done before merge.
- **Confirmed against the live cluster** in a throwaway namespace
  (`agent-fleet-obstest`, since deleted), which is the only way the
  remaining claims could be made:
  `serviceMonitorSelector`/`serviceMonitorNamespaceSelector` are both `{}`
  on the live Prometheus CR (all ServiceMonitors, all namespaces, no
  `release:` label) while `podMonitorSelector` **does** require
  `release: platform-prometheus` — so a PodMonitor would have been the wrong
  choice; the target reached `health: up` with `lastError: ""`;
  `agentfleet_tasks_current{repo,status}` reached Prometheus ~20s after a
  task was created; all four Grafana panel expressions returned series; and
  `QueryMetrics`/`GetFleetTopology` answered from the real Prometheus,
  confirming the `PROMETHEUS_URL` default resolves. Note the reload lag: the
  Operator writes the scrape job into its generated config immediately, but
  the running Prometheus is one config behind until the Secret mount
  propagates — roughly 20s here. An empty `up` right after applying a
  ServiceMonitor means "not reloaded yet", not "not matched"; check the
  generated config secret before concluding the selector is wrong.

## Alternatives rejected

- **`additionalScrapeConfigs` for annotation-based discovery.** Would make
  the original annotations work, but it is a change to `infra-bootstrap`'s
  platform values affecting every namespace in the cluster, to avoid writing
  two ServiceMonitors. Wrong blast radius.
- **A Pushgateway for worker pods.** The textbook answer for ephemeral
  jobs, and it would let per-session SDK counters into Prometheus. Rejected:
  it is a new stateful component to run and garbage-collect, for data that
  is already in Loki, queryable, with the retention story already solved.
- **Giving `core` pod-read RBAC for the topology.** Directly contradicts
  ADR-0020 point 1, and buys nothing `tasks.pod_phase` doesn't already
  carry.
- **Grafana embedded in an iframe instead of a topology view.** Cheaper, but
  the one thing worth building here is precisely the thing Grafana cannot
  do: click a cell, land on that session.
