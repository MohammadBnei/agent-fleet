---
name: fleet-feature
description: Checklist for adding functionality to agent-fleet itself (core/, provisioner/, sidecar/, or worker/) — new gRPC method, new MCP tool, new slash command, new task status, new planning-phase behavior. Use when the user wants to extend the fleet's own code, not a target repo it manages.
user-invocable: true
allowed-tools:
  - Read
  - Edit
  - Write
  - Bash(bun run typecheck)
  - Bash(go build *)
  - Bash(go vet *)
  - Bash(git diff *)
  - Bash(git status *)
---

# /fleet-feature — extend agent-fleet's own core/provisioner/sidecar/worker

This is for changing this repo's own source, not for a task a worker runs
against a target repo. See `docs/ARCHITECTURE.md` for the full flow and
`docs/adr/` for why things are shaped the way they are before changing
them — in particular `docs/adr/0019`–`0021` for why there's a
`core`/`provisioner`/`sidecar` split at all and what each owns.

**Reminder of the trust boundary before touching anything**: `core` is the
fleet's sole Postgres-credential holder and the only component that ever
calls the provisioner (hub-and-spoke, `docs/adr/0020`). If a new feature
seems to want a second component reaching Postgres directly, or a
sidecar/worker reaching the provisioner directly, that's very likely the
wrong shape — route it through `core`'s `CoreService` instead, even if it
feels like an extra hop.

## Adding a new agent-facing MCP tool

Agent-facing tools live in `sidecar/internal/mcpserver/server.go` and
proxy onward to `core` over gRPC — there is no direct network MCP surface
anymore (`docs/adr/0020` point 6, MCP is local-only).

1. Add the RPC to `proto/agentfleet/v1/core.proto`'s `CoreService`, run
   `buf generate` from `proto/`, commit the regenerated `proto/gen/go` and
   `worker/src/gen`/`dashboard/src/gen` (CI's generate-drift check fails
   otherwise).
2. Implement the handler in `core/internal/coreserver/server.go`.
3. Add the client method to `sidecar/internal/coreclient/client.go`.
4. Register the MCP tool in `sidecar/internal/mcpserver/server.go`'s
   `New()` (`s.AddTool(...)`) with a handler that calls the new
   `coreclient` method.
5. **Allowlist it in `worker/src/planning.ts`'s `query()` call's
   `allowedTools`** (as `mcp__agent-fleet-sidecar__<toolName>`, or covered
   by the existing `mcp__agent-fleet-sidecar__*` wildcard entry) — a tool
   missing from `allowedTools` isn't just unavailable, its permission
   request is *silently denied* (`canUseTool` in `runTask` only ever fires
   for `Write`/`Edit`; anything else not on `allowedTools` is denied with
   no visible signal). This exact gap burned a real session on denied
   calls with zero transcript output (`docs/adr/0008`).

## Adding a new wrapper-facing (non-agent) call

Everything the TS wrapper needs that the agent never decides to call
(housekeeping, control-flow) goes through the **other** local surface —
`sidecar/internal/localapi/server.go`'s plain HTTP/JSON API, not the MCP
server. Same proto/coreserver/coreclient steps as above, but the new
endpoint is a `mux.HandleFunc` in `localapi.New()`, and the TS-side caller
goes in `worker/src/sidecarClient.ts`, not `worker/src/planning.ts`'s
`mcpServers` config.

## Adding a new Discord slash command

Add to `core/internal/discord/commands.go`'s `commandDefs` slice and a
`case` in `core/internal/discord/handlers.go`'s `onInteractionCreate`
switch. Commands are registered guild-scoped (derived from the trigger
channel's guild, in `session.go`'s `onReady`) on session open, not
globally — so a new command shows up on next `core` restart, not after up
to an hour. If registration silently fails, check `core`'s logs for
`register command failed` and confirm the bot has the
`applications.commands` OAuth2 scope.

## Adding a new task status

Add a new `db/migrations/000N_*.up.sql`/`.down.sql` pair that `DROP
CONSTRAINT IF EXISTS`/`ADD CONSTRAINT`s `tasks_status_check` with the new
value included — never edit an already-applied migration file in place
(docs/adr/0030). Applied via a `PreSync` ArgoCD hook on every sync of
`core`'s Application, running the dedicated `migration` image (golang-migrate
against `db/migrations/` — `core` itself no longer embeds or applies any
schema, see `migration/Dockerfile`).
Also check `core/internal/coreserver/server.go`'s `terminalTaskStatuses`
map (`SetTaskStatus`'s opportunistic teardown trigger) if the new status
should (or shouldn't) trigger worker-pod/e2e-session cleanup.

## Adding a new planning-phase behavior (new checkpoint, new guardrail, etc.)

Read `worker/src/planning.ts`'s `runTask` in full first — it's **one
continuous streaming-input `query()` session** spanning planning and
implementation (`docs/adr/0021`), not two separate phases. The load-bearing
pieces: `InputQueue` (feeds `streamInput()`), the `canUseTool` callback
(the live `approved`-flag gate on `Write`/`Edit`), the human-message
consumer (`sidecar.streamHumanMessages`, which detects `/approve`/`/stop`
and calls `query.setPermissionMode()`/`query.interrupt()` live), and the
`for await (const msg of q)` loop's round-cap check (`PLAN_READY:`-prefixed
`send_message` calls, counted against `MAX_PLANNING_ROUNDS`). There is no
`runPlanningPhase`/`runImplementationPhase` split anymore — don't
reintroduce one. Any new guardrail should default to **unbounded/opt-in**,
not a fixed cap — `docs/adr/0008` documents why fixed defaults were all
tried and repeatedly proved too tight for genuine exploration.

**Interview/doubt gating logic belongs in `plannerPrompt`, not the message
loop.** Whether `architecture-interview`/`doubt-driven-development` run is
the planner's own judgment call, described in its prompt text — the
`for await` loop only sees `PLAN_READY:`-prefixed `send_message` posts
(the round-cap counting convention) and has no visibility into which
pipeline stages actually ran for a given round. Don't add a new
boolean/env flag to gate this — `docs/adr/0017` deliberately dropped the
old `skip_critique` `/task`-time knob in favor of the planner deciding per
task, with Mohammad able to interject live in the thread instead (now
delivered live via `streamInput()`, not polled).

## Before committing

- `cd worker && bun run typecheck && bun run test` — the SDK/sidecar-client
  mocks in `worker/src/planning.test.ts` are the load-bearing coverage for
  any `runTask` change.
- `cd core && go build ./... && go vet ./...` / same for `provisioner`/
  `sidecar` — `golangci-lint run` and `go test -race ./...` too if the
  change is non-trivial (see `.github/workflows/go.yml` for the exact CI
  gate, including `-tags=integration` tests against a real Postgres).
- If `proto/` changed: `cd proto && buf lint && buf generate` and commit
  the regenerated output — CI's drift check fails on an uncommitted diff.
- If the change touches `worker/Dockerfile`, `core/Dockerfile`,
  `provisioner/Dockerfile`, or `sidecar/Dockerfile`, remember all four
  build from the repo root as context (`docker build -f worker/Dockerfile
  .`, not `docker build worker/`) — `proto/gen/go` is a local Go module
  each Go service's `go.mod` doesn't `require` explicitly, relying on
  `go.work` locally; Docker builds need whatever `COPY`/module-cache steps
  the existing Dockerfile already has for it, don't assume `go mod tidy`
  will resolve it over the network.
