# ADR-0013: Go `fleet-core` replaces Redis coordination; `e2e-provisioner` and `bot` rewritten in Go

**Status:** Accepted
**Date:** 2026-08-03

## Context

The Redis durable-list pattern (ADR-0001) solved message loss, but every
reader (`mcp-redis`, worker's `watchBatch`/`waitForCheckpointReply`, `bot`'s
direct `RPUSH`) independently hand-rolled its own polling/cursor logic
against a raw Redis client — three different processes, three languages'
worth of the same coordination primitive, and no schema at all: the
planning transcript lived only in a Redis list, never in Postgres.
Meanwhile `e2e-provisioner` is the only RBAC-holding service in the fleet,
and `bot`'s `/e2e-kill` already violated the fleet's own "no service writes
another service's tables" instinct by writing `e2e_sessions` directly
instead of asking `e2e-provisioner`.

This was designed through an architecture interview and an adversarial
(doubt-driven) review before any code was written. Mid-interview, a
broader signal surfaced beyond "Redis is unmaintainable": a pattern across
recent work of shipping without verifying load-bearing assumptions
(`tools/list_changed` behavior never confirmed live, the e2e flow from
ADR-0012 never exercised end-to-end, a Bun/TLS incompatibility that reached
production as a CrashLoop, a PVC migration that hit two unexpected snags).
Mohammad explicitly chose to proceed with this rewrite now rather than
pause to verify the current system first — several ideas below were
rejected for concrete reasons during that review, kept as Alternatives
rather than re-derived from scratch here.

## Decision

Drop Redis entirely. A new Go service, **`fleet-core`**, owns Discord relay
(`bot`'s current role), planning-transcript coordination (`mcp-redis`'s
current role), and Loki log/introspection queries, as internal Go packages
in **one** binary/Deployment/ServiceAccount — these three share one trust
boundary (no cluster RBAC), so they don't get separate services;
decomposition in this fleet follows RBAC/trust boundaries only, never
feature boundaries. **`e2e-provisioner`** is rewritten in Go/`client-go`,
unchanged in role and RBAC scope (ADR-0012's invariants hold exactly:
namespaced `Role`, never `ClusterRole`; the sole holder of Pod/Service/
IngressRoute/Middleware verbs in the fleet). **`worker`** stays TS/Bun (sole
host of `@anthropic-ai/claude-agent-sdk`'s `query()`) and becomes an MCP
client of both — the same transport it already used for `e2e-provisioner`.

**The crux question, answered concretely: gRPC never touches `worker` on
any wire.** The Agent SDK can only receive tool definitions via MCP
transports (stdio/http/sse), never raw gRPC. `worker` ↔ `fleet-core` and
`worker` ↔ `e2e-provisioner` are both MCP-over-HTTP — `fleet-core` exposes
`send_message`/`wait_for_messages` with the **exact same tool contract**
`mcp-redis` used, to minimize prompt churn in `worker/src/planning.ts`.
`worker`'s own orchestration code (its poll loops, not the LLM sessions)
also became a raw MCP client of the same endpoint (`worker/src/
fleetCoreClient.ts`), reusing one surface for both callers rather than
inventing a second API for the worker-process case. gRPC is used exactly
once, internally, Go↔Go: `fleet-core` → `e2e-provisioner`, for
`KillE2eSession`/`GetE2eSessionStatus` — the mechanism for `/e2e-kill` (now
a `fleet-core` Discord handler) to stop writing `e2e_sessions` directly and
instead ask its owner. No gRPC server is stood up inside `fleet-core`
itself — there's no real second caller of one yet.

The planning transcript moves to a new Postgres table
(`planning_transcript`), read via a **pull/cursor API**
(`ReadSince(taskID, sinceSeq)`) that mirrors `LRANGE`-from-index — not a
bare gRPC streaming-watch RPC, which would silently drop messages across a
client reconnect (no durable resume-cursor protocol), reintroducing exactly
the failure mode ADR-0001 exists to prevent. Every mutating call carries a
client idempotency key; the enforcement lives in `PostgresStore.Append`
itself (not duplicated in each caller) — an empty key is treated as "not
supplied" and gets a fresh UUID minted before the query runs, so no two
calls can accidentally collide on the same empty-string key. (This was not
a hypothetical: a concurrency integration test written against a real
Postgres container caught exactly this collision — every concurrent append
with an empty key returned the same seq — before the fix moved the
minting into the store itself.)

A `.proto` schema (buf-managed: lint + breaking-change CI + a generate/
drift check) defines the `e2e.proto` service used for the one real gRPC
call, and a `transcript.proto` message set mirroring the MCP JSON payload
shapes for documentation purposes — worth noting honestly: the MCP wire
format uses a plain string for the transcript entry's `type` field
(`"discussion"|"approve"|"abort"|""`, matching today's existing
convention), not the protobuf enum `transcript.proto` defines for the same
concept. The `.proto` message isn't literally serialized over MCP; it
exists to document the shape and give a path to real codegen consumption
if a non-MCP client ever needs one. `ts-proto` runs in types-only mode
(`outputClientImpl=false`) for the one place generated types *are*
consumed: `worker`'s `e2e.proto`-adjacent gRPC message shapes, not the
transcript's loosely-typed MCP payload.

`bot`'s Discord relay folds into `fleet-core` rather than staying a
separate Deployment: no RBAC/trust-boundary difference justifies splitting
it out, and this is a single-operator homelab fleet with no uptime SLA —
the independent-restart-cadence argument for keeping it separate doesn't
clear the bar here (see Consequences for the SPOF trade-off this accepts).
`relayHumanMessage`'s network write (bot → Redis) becomes a plain in-process
Go function call (`discord` package → `transcript` package), since both
live in the same binary now.

Cutover is big-bang: no live production traffic to protect, so no
incremental/strangler rollout. Precondition, checked immediately before
flipping: zero `tasks` rows in any non-terminal status (`pending`,
`claimed`, `planning` — not just `pending`), since any task with a live
Redis transcript at flip time has nothing that migrates it forward.

## Alternatives considered

- **Bare gRPC streaming-watch RPC instead of a pull/cursor API** —
  rejected: a gRPC stream doesn't survive a client reconnect without an
  explicit resume-from-durable-cursor protocol, reintroducing the exact
  pub/sub message-loss failure mode ADR-0001 was written to prevent.
- **Redis Streams instead of a plain list** (native `XACK`/`XCLAIM`/
  `XAUTOCLAIM` — gets ack/retry/DLQ with zero language change) — rejected:
  doesn't serve the stated maintainability/DX goal driving this decision;
  the point was a typed contract, not just better delivery semantics.
- **Postgres `SKIP LOCKED` queue, staying in TS** — rejected: solves
  durability but not the DX/contract-clarity goal.
- **`bot` as its own Go Deployment, separate from `fleet-core`** —
  considered for independent restart/deploy cadence of the user-facing
  Discord connection; rejected as insufficient justification given no
  differing RBAC boundary and no uptime SLA in this fleet today.
- **Splitting Discord/transcript/logwatch into three separate services** —
  rejected: this fleet's own decomposition rule is RBAC/trust-boundary
  only; these three share one (no cluster RBAC), so splitting them buys
  isolation nobody needs at 3x the deployments/images/CI legs. Loki
  queries specifically need no k8s RBAC at all — querying Loki's LogQL API
  is just an HTTP call to an in-cluster Service.
- **`connect-es`/`@bufbuild/protobuf-es` for the TS side** — rejected:
  `worker` never speaks gRPC/Connect on any wire; a full RPC-client codegen
  for data that only ever travels as MCP JSON would be dead code.
- **`fleet-core` exposing its own gRPC server defensively, "for later"** —
  deferred, not built: no real second internal client of `fleet-core`
  exists yet; add it when one does.
- **Keeping Redis alongside gRPC** ("gRPC replaces Redis for exchanges
  between parts") — rejected: gRPC replaces the *transport* role Redis
  played, but not the *durability* role; a coordinator holding state only
  in memory would lose exactly the in-flight approval-gate state ADR-0001
  protects against on a crash. Since Postgres already persists every state
  transition synchronously, keeping Redis too would be redundant infra,
  not added safety.
- **Incremental/strangler rollout** (dual-write Redis+Postgres, cut over
  per-repo) — rejected per explicit instruction: no live production
  traffic exists to protect right now, so a transitional dual-write path
  buys nothing.
- **mockery/gomock generated mocks for the new Go services** — rejected:
  the interface surface (a 2-method transcript store, a ~6-function k8s
  wrapper, a handful of reconcile-loop dependencies) doesn't justify a
  codegen mocking framework; hand-written fakes plus `client-go`'s own fake
  clientset and `bufconn` for gRPC cover it with less machinery.
- **Pausing the rewrite to verify the current TS system first** — raised
  explicitly mid-interview once the "fleet feels shaky, not fully
  mastered" signal surfaced; declined — proceeding with the rewrite now was
  the deliberate choice, not an oversight.

## Consequences

- `fleet-core` becomes a single point of failure for Discord ingress,
  planning coordination, *and* log/introspection queries simultaneously —
  an accepted trade-off given this fleet's operating context (single
  operator, no SLA, `Recreate` deploys already tolerated everywhere), not a
  gap being deferred to fix later.
- `bot/` and `mcp-redis/` are deleted, not archived, in the same change as
  the cutover — git history is the reference copy if ever needed.
- This repo goes from zero Go code to two Go services in one change — Go
  module layout (`go.work`), linting (`golangci-lint`), and test
  conventions (table-driven + hand-written fakes + `client-go`'s fake
  clientset + `bufconn` + `testcontainers-go`-backed integration tests
  gated behind `-tags=integration`) are all established here from scratch,
  not inherited from an existing pattern.
- `buf`'s lint/breaking-change CI and the generated-code drift check are
  new toolchain surface for contributors — real but modest learning curve.
- `discordgo` has real feature-parity gaps against `discord.js` in edge
  cases (interaction edge cases, some gateway event coverage) — acceptable
  for this fleet's slash-command-and-thread-relay-only usage, but worth a
  real smoke pass against all four commands before declaring parity.
- A full distributed-system-level e2e/integration test harness is
  explicitly **not** designed here — the `testcontainers-go` integration
  tests added for `fleet-core`/`e2e-provisioner` are a natural first step
  toward one (they already exercise real Postgres transaction semantics
  for the exact tables coordination depends on, and caught one real
  concurrency bug during development), not a substitute for designing a
  proper one later.
- Cross-repo image-tag drift for `e2e-provisioner` continues (built here,
  deployed from infra-bootstrap manifests outside this repo's CI
  auto-bump) — unchanged from ADR-0012's own accepted trade-off, only the
  binary's language changed.
