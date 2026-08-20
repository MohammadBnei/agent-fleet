# ADR-0059 — core's `/metrics` moves off the gated port

- **Status:** Accepted
- **Date:** 2026-08-20
- **Related:** [#230](https://github.com/MohammadBnei/agent-fleet/issues/230)
  (`TargetDown`: every core target refused),
  [ADR-0056](0056-the-console-gate-is-oidc-in-core.md) (the gate that refuses
  them; amended here),
  [ADR-0047](0047-metrics-scoped-to-the-hubs.md) (what `/metrics` carries and how
  it is scraped; amended here)

## Context

`TargetDown` fired for `agent-fleet-core` with the pod healthy and serving the
console normally. The app was refusing its own scrape.

Three facts, each individually reasonable, combined into the outage:

1. `run.go` registers `/metrics` on the single HTTP mux, and wraps that whole mux
   in `OIDC.Gate` — deliberately, so that a route added later fails closed rather
   than defaulting open (ADR-0056 decision 5).
2. `exemptPrefixes` lists `/healthz`, `/webhook/alertmanager` and `/auth/`.
   `/metrics` is absent, also deliberately.
3. The ServiceMonitor scrapes `port: http` — 8080, the gated port — with no
   credential, because none was thought necessary.

A Prometheus scrape carries no session cookie, no `Sec-Fetch-Mode` and
`Accept: */*`, so it is not a document request either. It lands on the gate's
last branch and gets `401 unauthenticated`. Every scrape, since the OIDC work
merged on 2026-08-19. Metrics had worked from ADR-0047 until then.

The interesting part is the comment that justified (2). It read:

> `/metrics` is deliberately ABSENT. It is scraped in-cluster by a ServiceMonitor
> and has no IngressRoute, so it needs no exemption here.

The second clause was **false**, and the same file's own ingress comment says so:
this IngressRoute matches on **host with no path constraint**, so every path core
serves on 8080 is internet-routed. `/metrics` was reachable from the internet and
gated; the scrape was in-cluster and gated too. The gate cannot tell the two
apart — that is precisely the property ADR-0056 chose it for, over a Traefik
middleware that never sees a pod-network caller (#200).

So the endpoint was in the worst of both positions, and the obvious fix would
have moved it to the second-worst: adding `/metrics` to `exemptPrefixes` un-gates
it for Prometheus *and* for the internet. `fleet.bnei.dev` is still grey at
Cloudflare with `originLock` disabled, so there is nothing else in front of it.
What leaks is not nothing: `agentfleet_tasks_current{repo}` names every target
repo, `agentfleet_grpc_requests_total{method}` names every RPC served,
`agentfleet_max_in_flight` gives the fleet's size, and `promhttp` adds Go build
info for free. `core/internal/auth/auth_test.go` already asserted `/metrics` stays
gated, for this reason.

## Decision

**Serve `/metrics` on its own listener — `CORE_METRICS_PORT`, default 9091 —
exposed on the Service as `extraPorts.metrics`, and point the ServiceMonitor at
that named port.**

Nothing is exempted. `/metrics` stays gated on 8080 in the sense that matters: it
is no longer served there at all, so an external caller gets the SPA's 404-shaped
`index.html` behind the login, and the in-cluster scrape reaches a port no
IngressRoute routes to.

This makes the original comment's claim true instead of working around it. It is
also the shape the provisioner has had since ADR-0047 — `/metrics` on a port with
no ingress, no gate, no credential — so core now matches its sibling rather than
being the exception.

Cost is one goroutine and one `http.Server`. It is closed rather than gracefully
shut down at exit: a half-delivered scrape is worth nothing, and the drain that
matters belongs to the console's server.

## Alternatives rejected

- **Add `/metrics` to `exemptPrefixes`** — what #230 proposed. One line, fixes
  the scrape, and publishes the endpoint. Rejected on the host-only IngressRoute
  above. The issue's own root-cause analysis is correct; only its conclusion
  assumed a path constraint that does not exist.
- **A bearer token on `/metrics`, plus `bearerTokenSecret` on the
  ServiceMonitor** — the `/webhook/alertmanager` shape, and the repo's
  established answer for "in-cluster caller with no browser cookie". It works and
  it fails closed. Rejected as the wrong price: it keeps the endpoint
  internet-routed and buys back only credentialed access, while costing an
  Infisical secret, a k8s Secret the Operator can read, and a rotation story — for
  a problem a port number solves outright. The webhook needs a token because
  Alertmanager posts to it *through* the ingress; Prometheus does not.
- **A Traefik path rule denying `/metrics` on this host** — lives in
  `infra-bootstrap`'s `common-app-chart`, which this repo does not own, and #230
  is explicit that no infra-side change is wanted. It also re-introduces exactly
  the split ADR-0056 rejected: an ingress-level opinion about a caller that may
  never reach the ingress.
- **A NetworkPolicy restricting 8080** — constrains topology instead of
  establishing identity, already rejected in ADR-0056's alternatives, and does
  nothing about the internet-routed half.

## Consequences

- **`/metrics` is no longer on 8080.** Anything that scraped it there — a
  `kubectl port-forward 8080` habit, a hand-written scrape config — must move to
  9091. Nothing in this repo did except the ServiceMonitor.
- **The named port is load-bearing.** The Operator resolves `port: metrics`
  against the rendered Service; if `extraPorts.metrics` were ever dropped from
  `k8s/core.yaml`, the ServiceMonitor selects nothing and produces no `up` series
  at all — a silent no-op that looks like a working scrape until someone looks.
  Verification is a rendered Service, not a values file read.
- **ADR-0056's rule is unchanged and still the one to read before adding a
  route:** a new exempt path is a new public endpoint. This ADR removes a
  candidate for exemption rather than granting one.
- **ADR-0047's table moves**: core's `/metrics` is `:9091`, the provisioner's
  stays `:8080`. The metrics themselves, their names and their labels are
  untouched.
- The verification bar from ADR-0047 applies to this change too: a metric
  observed with a **non-default value** on the live endpoint, not merely present.
