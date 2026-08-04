# ADR-0014: fleet-core as the web dashboard's backend

**Status:** Accepted
**Date:** 2026-08-04

## Context

The only interface into agent-fleet has been Discord (thread scrollback)
and `kubectl logs` — there's no way to see "what's the fleet working on, in
detail" or attach growing tool integrations to a task. This ADR adds a web
dashboard: a task-centric control panel (view tasks and transcripts,
approve/stop a task, kill an e2e session, jump into a task's live
`e2e-runner` code-server session) meant to grow over time, not a one-shot
read-only viewer.

This was designed through a `frontend-design-interview` (product/UI shape)
and an `architecture-interview` (backend shape), then stress-tested with an
adversarial `doubt-driven-development` pass before landing on the design
below. That process's first hypothesis assumed `fleet-core` wasn't deployed
yet and would need a Discord on/off flag to avoid double-handling `/approve`
`/stop` against a still-live `bot/` — that assumption was wrong: per
[`adr/0013`](0013-go-fleet-core-and-e2e-provisioner-rewrite.md), `fleet-core`
already **is** the fleet's sole Discord surface (`bot/` and `mcp-redis/`
were deleted in that same cutover), so no such flag is needed here — Discord
just keeps running exactly as `adr/0013` already established.

## Decision

`fleet-core` is extended with a new, additive surface: authenticated REST
routes for reads (`tasks`, `planning_transcript`, e2e-session-status) and
three write actions (approve, stop, kill-e2e), an SSE stream for live
task/transcript updates, and static-file serving of the built dashboard SPA
(`dashboard/`, React + Vite + TypeScript + Tailwind + DaisyUI) — all in the
same binary/pod `adr/0013` already established for Discord + MCP + Loki
queries. This is the first time that binary is reachable from outside the
cluster.

**Write actions call the same store methods Discord already calls** — no
new business logic. The dashboard's approve/stop/kill-e2e handlers invoke
`transcript.Store.Append(ctx, taskID, "human", text, msgType, key)` and
`e2eclient.Client.KillSession(ctx, taskID, key)`, identically to
`fleet-core/internal/discord/handlers.go`'s command handlers.

**Auth is a Traefik BasicAuth middleware** (`basic-admin-auth`, already
gating pgweb/Alertmanager) at the ingress layer — a single shared
credential, not per-user identity; this is a solo-operator homelab tool.
Given that, BasicAuth alone is insufficient for the three write routes:
browsers auto-attach cached Basic credentials to same-origin requests
regardless of which page triggered them, so a third-party page could
otherwise forge an approve/stop/kill call. Mitigation: each write route
requires a fixed custom request header (`X-Fleet-Dashboard`), and the
dashboard's API never emits `Access-Control-Allow-Origin`. A plain HTML
form or `<img>` tag can't set a custom header, and a cross-origin `fetch`
that tries triggers a CORS preflight this server doesn't allow — only
same-origin JS (the dashboard's own SPA) can successfully call these
routes. Each write also mints a server-side idempotency key
(`uuid.NewString()`), reusing the exact pattern `mcpserver.go`'s
`send_message` tool and `PostgresStore.Append` itself already enforce, so a
double-click or retried request can't double-append a transcript entry or
double-fire a kill.

**The SSE endpoint is one shared poller fanning out to subscriber
connections** (`internal/dashboard/hub.go`'s `Hub`), not one poller per
open browser tab — the untuned `pgxpool` (per `adr/0013`) already serves
the MCP `wait_for_messages` long-poll and the Discord relay loop. The hub
only polls tasks that currently have at least one live subscriber, reusing
`transcript.Store.ReadSince` — the same read path the REST transcript
endpoint and `worker/`'s MCP long-poll already use.

**`/mcp` keeps working unchanged** — `worker/src/fleetCoreClient.ts`
depends on it for the planning round-cap's `wait_for_messages` call;
nothing here relocates or wraps that handler.

**Accepted limitation:** `common-app-chart`'s `IngressRoute` template
matches on `Host(...)` only, with no `PathPrefix` support. Now that
`fleet-core`'s Service is reachable through a public, BasicAuth-gated
`IngressRoute` for the dashboard, `/mcp`/`/healthz` become reachable
through that same host too — not fully private, but still behind the same
shared credential as everything else on that hostname. A real fix would
need `common-app-chart` itself to support path-scoped `IngressRoute`
matching, which is out of scope for this feature.

## Alternatives considered

- **A separate dashboard-only backend with its own Postgres pool** —
  rejected: would duplicate `tasks`/`planning_transcript`/`e2e_sessions`
  query and action logic in a second place, splitting "who's allowed to
  write these tables" across two codebases instead of one.
- **Dashboard queries Postgres directly, no backend at all** — rejected:
  exposes raw DB credentials/schema to a public-facing app and bypasses the
  idempotency/relay guarantees already built into `fleet-core`'s transcript
  and e2e-kill paths.
- **A Grafana dashboard against Postgres/Loki** (already running in this
  homelab) — rejected: read-only by design, can't host the approve/stop/
  kill actions or the code-server link integration.
- **Polling instead of SSE** — a genuinely close call, since it would match
  `fleet-core`'s existing 2s-poll relay-loop pattern exactly with zero new
  transport code. Rejected in favor of SSE for a more live-feeling
  transcript/task view, accepting the added server-side stream-management
  complexity (mitigated by the shared-poller-fan-out design above).
- **A separate static-file-serving deployment for the SPA** — rejected:
  would split one logical dashboard release into two pods/Applications for
  no isolation benefit at solo-operator scale.
- **A `FLEET_CORE_DISCORD_ENABLED` on/off flag** — this was the original
  design, written before discovering `adr/0013`'s cutover was already
  live. Dropped entirely once confirmed: `fleet-core` is already the
  fleet's only Discord surface, so there's nothing to gate.

## Consequences

- `fleet-core` picks up a second, unrelated exposure: a public dashboard
  backend alongside its existing Discord/MCP/Loki responsibilities, in the
  same process. A bug or resource spike in the new dashboard code shares
  fault domain with `/mcp` (`worker/`'s round-cap dependency).
- `/mcp` is reachable through the same public, BasicAuth-gated host as the
  dashboard (see the named limitation above) — accepted for v1, revisit if
  `common-app-chart` ever gains path-scoped ingress matching.
- Write-action attribution in the transcript is "someone holding the
  shared BasicAuth credential," not a real per-user identity — acceptable
  at today's solo-operator scale; revisit if this ever needs multiple
  operators or an audit trail of who clicked what.
- This repo's second build step: `dashboard/`'s Vite/Tailwind toolchain,
  bundled into `fleet-core/Dockerfile` as an extra `spa` build stage ahead
  of the Go build, whose output is `go:embed`'d into the binary
  (`fleet-core/internal/webui`).
