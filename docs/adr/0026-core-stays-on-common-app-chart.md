# ADR-0026: `core` stays on `common-app-chart`, no per-app Helm chart

**Status:** Accepted
**Date:** 2026-08-05

## Context

`core`'s deploy config is split across two repos per ADR-0007's two-source
Application: the Helm chart comes from
`infra-bootstrap/gitops/platform/common-app-chart`, the values come from
this repo's `k8s/core.yaml`. Any deploy-*shape* change to `core` (not just
an image-tag bump) that touches the chart itself means a PR in
`infra-bootstrap` in addition to one here — standing friction that
prompted the question of whether `agent-fleet` should own its Helm
templates directly instead of reusing the shared chart.

Investigation found no concrete incident forcing this: `k8s/core.yaml` is
small (~35 lines of actual values) and uses only features the chart
already documents (`extraPorts`, `hooks.migrate`, `middlewares`) — no
workarounds, no fighting the abstraction. `provisioner` and `worker` did
leave Helm (plain manifests / client-go-built pod specs respectively), but
for a structurally different reason: cluster RBAC (`provisioner`) and a
dynamic multi-container pod spec (`worker`'s sidecar) that
`common-app-chart` cannot express at all. Neither applies to `core` today.
`common-app-chart` itself is shared by roughly ten real consumers across
the cluster, not just this repo.

## Decision

`core` keeps reusing `common-app-chart`. Two different kinds of future
need get two different responses:

- **Resource-level** (something the chart doesn't template at all, e.g. a
  new standalone object): use the chart's existing `extraManifests` values
  key from `k8s/core.yaml`. This stays a one-repo change — no chart PR in
  `infra-bootstrap` required.
- **Template-level** (changing the structure of an existing template
  itself, e.g. adding a second container to the Deployment the way
  `worker`'s sidecar needed): that is the actual signal to revisit this
  decision — the same bar `provisioner`/`worker` already cleared before
  leaving Helm. A standing worry about cross-repo friction, without a
  concrete template-level need, is not that signal.

Forking a full parallel Helm chart into `agent-fleet` preemptively is
rejected as the fix for either case.

## Alternatives considered

Forking `common-app-chart`'s templates into a bespoke chart owned by
`agent-fleet` — rejected. It would recreate the "N charts to maintain in
lockstep" cost that `infra-bootstrap`'s ADR-0004 (Pattern C) explicitly
rejected cluster-wide, without consumer-side evidence that `core` has
actually outgrown the shared chart.

## Consequences

- The two-PR cost of ADR-0007's two-source Application remains for genuine
  template-level changes — an accepted, shared cost, the same one roughly
  ten other `common-app-chart` consumers already carry.
- `extraManifests` is now the documented first stop for a new
  resource-level need on `core`, ahead of either a chart PR or a Helm
  opt-out.
- If a template-level need does arise, the existing `provisioner`/`worker`
  precedent (plain manifests / client-go, not a second Helm chart) is the
  pattern to follow, not a per-app chart.
