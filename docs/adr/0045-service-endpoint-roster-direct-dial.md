# ADR-0045 — Direct dial via a service endpoint roster

- **Status:** Accepted
- **Date:** 2026-08-13
- **Supersedes:** [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) point 4
  (partially — only the e2e tool-call relay; core still commands the
  provisioner for all pod lifecycle) and point 6 (MCP is no longer
  local-only)

## Context

Every sandbox tool call — each `run_command`, each Playwright action —
travels agent → sidecar (MCP, localhost) → `core` (gRPC) → `provisioner`
(gRPC) → e2e pod (MCP). At the `core` hop it is a **pure relay**:
`CallE2ETool` (`core/internal/coreserver/server.go:249`) opens no connection,
touches no database, and adds no state. It unwraps three fields and rewraps
them. `ListE2eTools` (`:241`) is the same. They are the only two methods on
`CoreService` that do this — every other one writes a row, resolves a
profile, checks a lease, or long-polls Postgres.

ADR-0020 point 4 chose that shape for consistency and wrote down its own
exit condition:

> Rejected in favor of full hub-and-spoke consistency — **may be revisited if
> that hop proves materially slow in practice**, but not pre-optimized for now.

That condition was never evaluated. This ADR evaluates it, and then decides
against the relay anyway — for reasons that are not latency.

### The measurement

Both hops already log `duration_ms` (`core/internal/coreserver/interceptor.go:16`,
`provisioner/internal/grpcserver/interceptor.go:16`) and Alloy already ships
them to Loki, so the number cost nothing to obtain. Seven days of production,
successful calls only:

| Method | `core` p50 / p95 | `provisioner` p50 / p95 | **hop cost** |
|---|---|---|---|
| `CallE2eTool` (n=22) | 751 / 3058 ms | 751 / 3058 ms | **0 ms** |
| `ListE2eTools` (n=27) | 17 / 32 ms | 17 / 32 ms | **0–1 ms** |

`core_p95 − provisioner_p95` is the entire cost of the relay, and it is
inside the measurement noise. At p50 the hop is **0.1%** of a tool call. For
scale, `SendMessage` — a call where core does real work, a transcript insert
— is 4 ms p50.

**So ADR-0020's trigger did not fire, and this ADR does not claim a latency
win.** The 751 ms p50 is the tool itself plus the MCP handshake at the far
end; the work that actually moves it is the mutex and DNS fixes below, which
are independent of the topology and ship first.

Saying this plainly matters more than it costs. The relay was suspected of
being slow, the suspicion was cheap to test, and it was wrong. An ADR that
quietly claimed the speedup it was reaching for would make the next
performance argument in this repo less trustworthy.

### What is actually wrong with the relay

Three things, none of them milliseconds.

**1. Every new service costs a proto pair, a relay handler, and a client
wrapper before anything can call it.** ADR-0035 hit exactly this and took a
*named exception* to hub-and-spoke for `thot`. [ADR-0037](0037-thot-is-a-worker-task.md)
revoked the exception — but read its reason carefully: thot failed because it
arrived as *"a second mental model: a standing Deployment with its own
`ThotService` gRPC, its own `thot_events` stream, its own bespoke dashboard
page, its own bearer token, and a named exception to the fleet's
hub-and-spoke rule."* The exception was one symptom among five. Direct dial
itself was never the thing that failed, and `thot-executor` still uses it
today (`provisioner/internal/k8s/ingredients.go:80-81` injects `EXECUTOR_ADDR`
+ `THOT_AUTH_TOKEN`; `executor/cmd/kubectlshim/main.go:54` dials it directly).

**2. A tool call routes through the fleet's Postgres-credential holder for no
reason connected to credentials.** ADR-0020 point 1 centralized
`AGENTFLEET_DB_*` in `core` and that is load-bearing. But `CallE2eTool`
touches no database. It inherits a constraint that exists to protect
something it never touches.

**3. `core` is in the path of work it has no stake in.** A `core` restart —
and `Recreate` deploys are this fleet's norm — stalls every live session's
build and test commands, not just its database work.

The measurement reframes these rather than weakening them: the relay is not a
performance problem, it is a **coupling** problem, and the fix is worth doing
on that basis alone.

## Decision

### 1. A service endpoint is data, carried on responses that already exist

```proto
message ServiceEndpoint {
  string name     = 1;  // "playwright" | "exec" | "executor"
  string address  = 2;  // "e2e-abc.agent-fleet.svc.cluster.local.:8932"
  string protocol = 3;  // "mcp-streamable-http" | "grpc"
  string path     = 4;  // "/mcp" for MCP, "" for gRPC
  string token    = 5;  // bearer; empty when structurally protected
}
```

`CreateE2eSessionResponse` and `RequestE2eEnvResponse` each gain `repeated
ServiceEndpoint endpoints`. Standing services arrive as one `FLEET_ENDPOINTS`
env var on the sidecar container, following the `EXECUTOR_ADDR` precedent.

**No `GetServiceRoster` RPC.** A lookup call through `core` would be a fresh
relay in the exact shape of the one being deleted — and `core` cannot compute
the answer anyway. The roster is a *field on a response*, not a subsystem.

Deliberately absent from the entry: **`who_may_call`** (authorization is a
NetworkPolicy; a field the caller reads is advisory), **`ttl`** (validity is
implied by delivery — a failed dial triggers re-resolution), and **`scope`**
(a roster reaches one sidecar serving one task; scope *is* the delivery
channel).

### 2. Kubernetes is the registry; the provisioner resolves it

Not a `services` table. Two reasons that fails: the provisioner holds no
database credentials and is the only component that knows a sandbox exists,
so registration would need a new `CoreService.RegisterService` — the relay
again, wearing a different name. And for pod existence **Kubernetes is ground
truth**, which `provisioner/cmd/provisioner/main.go:56` already states
outright. A row survives an OOMKill; a Service endpoint does not. The table
would be a cache that lies.

The address is a pure function the provisioner already computes
(`names.go:60-66`), the Service it already creates is the entry, and the
labels it already sets are the index. Standing and ephemeral services differ
only in *who computes `address`* — that is on the producing side, not in the
model.

### 3. Resolution is lazy, and the existing retry is the refresh

`runCommandHandler` is already *call first, provision on failure*. Under
direct dial: dial fails → `RequestE2eEnv` → roster returns in the response →
cache → redial. No TTL, no poll, no watch. Invalidate on dial error and on
`kill_env`.

### 4. Authorization is a per-task NetworkPolicy, not a token

`k8s/provisioner/networkpolicy.yaml` already protects the sandbox; today its
rule 3 (`app: provisioner` → 8931/8932) is the only thing keeping one task's
worker out of another task's MCP ports. That rule is replaced by a per-task
policy the provisioner creates beside the Service, admitting only
`podSelector{component: worker, task-id: <taskID>}`.

A bearer token was rejected on merits, not just on the structural-over-asked
ranking: it would require **the sandbox to hold a secret**, and
[ADR-0039](0039-e2e-pod-is-the-worker-sandbox.md)'s entire justification for
`run_command` being un-prompted while `Bash` is `canUseTool`-gated is that the
sandbox holds no fleet credentials — *"Adding credentials or widening its
mount reopens that decision."* NetworkPolicy keeps `e2e-runner` unchanged,
which also keeps it out of the image-skew matrix.

Policies are **additive**, so the narrow per-task rule composes with the
existing broad one rather than templatizing it. The corollary is a hazard
worth stating: a later broad rule silently widens every per-task policy.

### 5. `core` dials the sandbox too, for its own human-facing path

The sidecar is not the only caller. `core/internal/dashboard/server.go`'s
`runInE2ePod` backs two dashboard handlers (`GetE2EAppLog` and the run-command
surface added by [ADR-0044](0044-e2e-pod-outlives-the-app.md)) — a *human*
asking for a command in the sandbox, which has nothing to do with the agent.

`core` becomes an ordinary roster consumer rather than keeping a private
relay: `GetE2eSessionStatusResponse` gains the same `endpoints` field, and
`core` dials the sandbox with a small MCP client of its own. The static
NetworkPolicy — not the per-task one, since `core` is not task-scoped — gains
a rule admitting `core` to `:8932`.

This is the honest trade, and it should be stated rather than buried: **the
provisioner stops speaking MCP, and `core` starts.** The win is not "one
fewer component imports `mcp-go`" — it is that nobody is a *relay* any more.
Each component talks to the sandbox for its own reasons, and neither carries
the other's traffic.

The two clients are deliberately **not** factored into a shared package.
`core` and `sidecar` are separate Go modules under `go.work`; sharing ~20
lines would mean a new module or an import edge between two components that
otherwise have none. Duplicating the dial is cheaper than the coupling.

### 6. What is deleted

`CallE2eTool` and `ListE2eTools` from **both** `CoreService` and
`ProvisionerService`, their four message pairs, `E2eToolDescriptor`, both core
handlers, both provisioner handlers, the two `provisionerclient` wrappers, the
two `sidecar/internal/coreclient` wrappers, and **the entire
`provisioner/internal/mcpproxy` package**.

The provisioner stops importing `mark3labs/mcp-go` altogether — it no longer
speaks MCP. A `buildguard`-style import test pins MCP-client usage to the two
components that dial for their own reasons (`sidecar`, `core`) and keeps it
out of the provisioner, so a reintroduced proxy fails CI rather than review.

## Consequences

- **MCP is no longer local-only.** ADR-0020 point 6's "gRPC is the only
  inter-pod protocol" no longer holds; the sidecar speaks MCP to its task's
  sandbox. The boundary that replaces it is narrower and enforced by the CNI
  rather than by convention: *one* worker may reach *its own* task's sandbox,
  on two ports.
- **ADR-0020 points 1, 2, 3 and 5 are untouched.** `core` remains the sole
  `AGENTFLEET_DB_*` holder, still commands the provisioner for every pod
  lifecycle decision, still treats Postgres as concurrency ground truth, and
  the sidecar still holds exactly one outbound gRPC connection for everything
  else. ADR-0034's "worker pod holds zero Kubernetes RBAC" is also untouched —
  dialing a Service needs DNS and a network path, not RBAC.
- **Adding a service no longer requires touching `core`.** It requires a
  roster entry and a NetworkPolicy rule. This is the concrete answer to the
  cost that produced ADR-0035's exception.
- **Tool-call latency stops being visible in `core`'s logs.** The relay was
  incidentally an observability chokepoint; deleting it removes that. A
  client-side interceptor on the sidecar's gRPC connection lands *before* the
  cut-over, not after.
- **The cut-over is skew-sensitive.** `core`, `provisioner` and the worker
  images deploy independently, so the sidecar keeps a fallback to the relay
  while the roster is empty, and the deletion waits for the fleet to drain of
  pods running the older sidecar.
- **A 24% error rate on `CallE2eTool` is now visible** (7 of 29 calls over
  7 days, all `dial tcp: lookup …` failures against torn-down sandboxes).
  This ADR does not fix it; it is recorded because measuring for latency
  surfaced a reliability number nobody had looked at.
- **The retry discipline this needs already exists.**
  [ADR-0044](0044-e2e-pod-outlives-the-app.md) moved waiting client-side into
  `runCommandRetryDelays` (`sidecar/internal/mcpserver/server.go:429`), which
  already tells "no pod" from "pod exists, exec listener unreachable" and
  gives up with a legible error instead of looping. Direct dial must
  **preserve** that loop, not reinvent it — the failure mode it guards
  against (a refused dial read as "no sandbox", re-provisioned into an
  already-running pod, forever) is otherwise reintroduced the moment the
  sidecar owns the dial.

## Alternatives

- **Keep the relay** — rejected. The measurement removes the latency argument
  *for* deleting it, but nothing about 0 ms argues for keeping a passthrough
  that costs a proto pair per service and puts the database-credential holder
  in the path of a shell command.
- **A service mesh (Istio/Linkerd)** — rejected. It adds a proxy hop per call,
  making the one thing that was measured *worse*, and solves mTLS and traffic
  shaping, which are not the stated problems. The fleet's discovery need is
  already met by Kubernetes DNS.
- **A `services` table in Postgres** — rejected; see Decision §2. It would
  invert ground truth and require a new relay to populate.
- **Per-session bearer token instead of NetworkPolicy** — rejected; see
  Decision §4. It puts a credential in the sandbox that ADR-0039 depends on
  being absent.
- **Direct dial as a one-off exception for the e2e path** — rejected. That is
  precisely ADR-0035's shape, and ADR-0037's post-mortem is that a named
  exception is a second mental model. A roster makes direct dial the general
  rule with one delivery mechanism.

## Verification note

Cilium enforces NetworkPolicy on `ukubi-cluster`; **kind's default CNI does
not**. `/kind-local` can prove roster delivery and a successful dial and
nothing whatsoever about isolation. The negative test — task B's worker
refused at task A's `:8932` — is real-cluster-only, and runs in a scratch
namespace (`agent-fleet-scratch`, separate database, Discord disabled) rather
than against the live fleet. The separate database is not hygiene: a second
`core` on the production database runs its own dispatch loop against the same
`pending` rows and would claim real tasks.

For future queries: the provisioner's Loki streams carry
`service_name="provisioner"`, which is a cleaner selector than the
`job=~".*/provisioner-.*"` regex used elsewhere.
