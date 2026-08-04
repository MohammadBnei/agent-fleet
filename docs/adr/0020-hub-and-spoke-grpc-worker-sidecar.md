# ADR-0020: Hub-and-spoke gRPC between fleet-core/provisioner/worker sidecar — Postgres centralized, MCP kept local to the agent

**Status:** Accepted
**Date:** 2026-08-04

## Context

ADR-0019 defined three components (shared PVC, unified provisioner,
fleet-core) but not how they actually talk to each other. Two concrete
needs surfaced once that got interrogated: fleet-core needs live,
*pushed* visibility into provisioner-managed pod lifecycle — not just
on-demand pull — for logging and other not-yet-defined future purposes
that on-demand pull can't serve by definition; and "the monolith handles
Postgres exchanges" (established while naming components after ADR-0019)
needs to actually mean *only* fleet-core ever holds `AGENTFLEET_DB_*`
credentials, not each component keeping its own as needed.

Reached via the same live architecture interview as ADR-0019, continued
in one sitting. Before implementation started, this ADR (together with
ADR-0019) went through a doubt-driven adversarial review — a fresh-context
reviewer was asked to find gaps that would block a first end-to-end task,
not to validate. It found two real blockers (the TS worker's own
control-flow calls, e.g. the human approval-gate poll, had no specified
channel; this ADR's original text contradicted ADR-0019's on who claims
tasks) and two real correctness gaps deferred too casually (a shared-clone
data race once concurrency is fleet-wide; concurrency headroom with no
durable source of truth across a fleet-core restart). All four are folded
into the Decision below rather than tracked separately — the text already
reflects the corrected design, not the original proposal.

## Decision

1. **Postgres access is fully centralized in fleet-core.** No other
   component — provisioner, worker pod, or the sidecar below — ever holds
   `AGENTFLEET_DB_*` credentials. Every read/write from those components
   routes through a fleet-core gRPC service.

2. **fleet-core commands, the provisioner executes — never the reverse.**
   fleet-core's own internal loop watches for pending tasks and free
   concurrency headroom, claims a task (the `SKIP LOCKED` transaction runs
   *inside* fleet-core, never exposed as an RPC the provisioner calls),
   then calls `CreateWorkerPod(taskId, repo)` on the provisioner. This
   matches how `e2e-provisioner` already behaves today — reactive, told
   what to do via `request_e2e_env` — rather than the asymmetric
   autonomous-polling model ADR-0019 first proposed for worker pods only.
   **This formally supersedes ADR-0019 points 4 and 5** (provisioner claims
   the task itself; provisioner enforces the concurrency cap) — a
   contradiction between the two documents (ADR-0019 has the provisioner
   running a DB transaction directly, point 1 above forbids it from ever
   holding DB credentials at all) caught by this ADR's own doubt-driven
   review, not by design. ADR-0019 has been annotated to point here.

3. **The provisioner reports back via push, not pull**: one long-lived
   server-streaming gRPC call, provisioner as the streaming client,
   pushing pod-lifecycle events (created / scheduled / running / crashed /
   terminated) to fleet-core as they happen. fleet-core journals every
   event immediately (`knowledge_journal`) for logging and future use, but
   the dispatch decision itself (does headroom exist for another task)
   is computed from `SELECT COUNT(*) FROM tasks WHERE status IN
   ('claimed','planning','implementing')` — Postgres, not the stream, is
   ground truth for concurrency. This matters specifically because
   fleet-core restarts (`Recreate` deploys are already this fleet's norm,
   ADR-0013) would otherwise reset in-memory stream-derived state to zero
   with no reconciliation, either wrongly blocking dispatch or overcommitting
   past the cap — the same failure class `docs/DECISIONS.md`'s existing
   "no bare streaming-watch RPC without a durable resume cursor" rule
   exists to prevent (today scoped to the planning transcript; this is the
   same principle applied to a second stream). The event stream stays
   valuable for what pull-only couldn't do — live dashboard status, logging,
   future uses — it's just not the *only* source of truth for the one
   decision that has a durable one available already.

4. **Full hub-and-spoke: nothing talks to the provisioner except
   fleet-core.** This includes e2e-pod requests — today a direct
   worker→`e2e-provisioner` MCP relationship — which now route agent →
   local sidecar (MCP) → fleet-core (gRPC) → provisioner (gRPC) → e2e pod,
   including live Playwright tool-call proxying during an active e2e
   session.

5. **Every worker pod gets a small Go sidecar — a real second container**,
   not a second process sharing the main container. This costs nothing
   extra here specifically because these pods are no longer
   Helm/`common-app-chart`-deployed at all (ADR-0019): the provisioner
   builds the Pod spec directly via `client-go`, where a second container
   is exactly as easy as one. The sidecar has **three** independent jobs
   — an earlier version of this ADR only named two, and its own
   doubt-driven review caught that the third has nowhere else to live:
   - Hosts a local (`localhost`-only) MCP server the Agent SDK session
     connects to, proxying every agent-*initiated* tool call
     (`send_message`, `wait_for_messages`, `AskUserQuestion`,
     `request_e2e_env`, future `TodoWrite`) onward to fleet-core over gRPC.
   - Independently computes and pushes git diff/branch/elapsed-time/
     tool-call-summary telemetry to fleet-core over the same gRPC channel,
     on its own schedule — entirely decoupled from what the agent itself
     chooses to call. This replaces the direct-Postgres-write mechanism
     floated earlier in this same session, now moot under point 1.
   - **Hosts a second, plain local API (not MCP-shaped) for the TS worker
     process's own control-flow and housekeeping calls** — everything
     `worker/src/planning.ts`/`db.ts`/`index.ts` do today outside of any
     agent tool call: `claimNextTask`-adjacent bookkeeping, heartbeat,
     status transitions, `appendJournal`, `saveSessionIds`, the
     stale-lease check immediately before the irreversible push/PR
     (`stillHoldsLease`), and — the load-bearing one — delivering every new
     human message (steering text, `/approve`, `/stop`) to the wrapper
     *live*, for the wrapper to feed into the running Agent SDK session via
     `streamInput()`/`interrupt()`/`setPermissionMode()`. **This last part
     was originally described here as a poll deciding continue-vs-
     `abortController.abort()`** — corrected by ADR-0021, which found the
     SDK's real streaming-input primitives and designed the actual
     mechanism; this bullet exists to name that the responsibility lives on
     the sidecar, ADR-0021 is authoritative on how it works. This
     responsibility is genuinely distinct from the agent-facing MCP surface
     above: none of it is something the agent decides to do, all of it is
     the TS wrapper's own event-driven orchestration, and without an
     explicit channel for it ADR-0005's entire human-approval gate has
     nowhere to run. Both this and the MCP surface funnel through the same
     single outbound gRPC connection the sidecar holds to fleet-core — two
     local entry points, one upstream channel.

6. **MCP is purely local** (agent ↔ its own pod's sidecar, over
   `localhost`). **gRPC is the only inter-process/inter-pod protocol
   anywhere in the fleet.** fleet-core's MCP-over-HTTP server — today
   reachable directly by the agent for `send_message`/`wait_for_messages`/
   `AskUserQuestion` (ADR-0013) — is retired in favor of exposing these as
   gRPC services the sidecar calls on the agent's behalf.

## Alternatives considered

- **Pull-only provisioner status**, matching today's `GetE2eSessionStatus`
  pattern. Rejected: doesn't serve logging or not-yet-defined future needs
  — those specifically require knowing about events as they happen, which
  an on-demand pull structurally can't provide.
- **Provisioner polls/claims tasks itself** (a `ClaimNextTask` RPC it
  calls on fleet-core, ADR-0019's original proposal). Rejected:
  inconsistent with `e2e-provisioner`'s existing reactive, commanded
  behavior, and splits "decide whether to spawn" across two components
  instead of leaving it entirely in one.
- **Keep worker→`e2e-provisioner` direct for live Playwright proxying**,
  considered given the extra hop's latency cost for interactive browser
  automation mid-session. Rejected in favor of full hub-and-spoke
  consistency — may be revisited if that hop proves materially slow in
  practice, but not pre-optimized for now.
- **Sidecar as a second process inside the main container** (lower
  perceived effort). Rejected: since the Pod spec is already built
  directly in Go by the provisioner rather than through a Helm chart,
  there's no actual effort saved by cramming two processes into one
  container — only a worse operational boundary (can't restart/monitor
  the sidecar independently of the agent's own process).
- **Route worker↔fleet-core housekeeping calls through the existing
  agent-facing MCP tool-call shape** (this session's own earlier
  proposal, before Postgres centralization was decided). Rejected:
  conflates a protocol built for LLM tool invocation with plain
  infrastructure telemetry the agent never decided to send — the sidecar
  split keeps those cleanly separate.
- **Leave the TS wrapper's control-flow calls (approval-gate polling,
  heartbeat, status, lease-check) unspecified and figure it out at
  implementation time.** Rejected after this ADR's own doubt-driven
  review: this isn't a detail implementation can safely invent — without
  it, ADR-0005's approval gate has no way to ever fire, which blocks a
  first end-to-end test outright, not just a rough edge. Addressed by
  point 5's third sidecar responsibility.
- **Derive concurrency headroom purely from the provisioner's event
  stream, no independent source of truth** (this ADR's own original
  point 3). Rejected after doubt-driven review flagged it as the same
  durable-cursor problem `docs/DECISIONS.md` already has a rule against,
  just recurring on a stream this ADR introduced. Addressed by sourcing
  the dispatch decision from Postgres directly (point 3, revised).

## Consequences

- `fleet-core/internal/mcpserver`'s HTTP MCP server, today reachable
  directly by the agent, is retired — replaced by gRPC services the
  sidecar calls, plus a new local MCP server living in the sidecar binary
  itself (new component, e.g. `worker-sidecar/`).
- `worker/src/db.ts` (direct Postgres) and `worker/src/fleetCoreClient.ts`
  (direct remote MCP client) both go away; the TS worker process talks
  only to its own pod's `localhost` sidecar — over two different local
  surfaces (MCP for the agent, plain API for the wrapper's own
  control-flow), not one.
- fleet-core needs a `SELECT COUNT(*)`-based headroom check alongside the
  event-stream ingestion path — a second, independent way of answering
  "how much concurrency is free," not a replacement for the stream (see
  point 3).
- `e2e-provisioner`'s own MCP surface (today's `/mcp/:taskId`, directly
  reachable by the worker per ADR-0012) is retired in favor of fleet-core
  proxying — a real behavior change to how live Playwright tool calls
  flow during an e2e session, accepted as the cost of full consistency
  (see Alternatives). **ADR-0012's text "The worker only ever talks to it
  as an MCP client over the network" is superseded by this point**; the
  rest of ADR-0012 (RBAC boundary, `NetworkPolicy`, path routing, GPU
  deferral) is unchanged.
- The provisioner's `client-go` Pod-spec construction changes: every
  worker pod it creates now has two containers, not one.
- fleet-core gains a new event-stream ingestion path (the provisioner's
  pushed pod-lifecycle events) and a new set of gRPC services for the
  sidecar to call — real new surface area, not a relabeling of what
  exists today.
- ADR-0017/0018's decisions (single-session planner; `AskUserQuestion`
  answered via the dashboard) are unaffected in substance — the agent
  still calls the same-named tools with the same semantics. Only the
  transport underneath changes (proxied through the local sidecar instead
  of a direct remote HTTP call).
- **Deferred, explicitly out of scope here**: exact gRPC service/message
  definitions (proto-level implementation, not this decision); what the
  "not-yet-defined future purposes" for provisioner event logging turn out
  to be — left open on purpose, not designed now.
