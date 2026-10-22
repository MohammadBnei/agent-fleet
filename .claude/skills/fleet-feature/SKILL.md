---
name: fleet-feature
description: Checklist for adding functionality to agent-fleet itself (core/, provisioner/, sidecar/, worker/, islands/, definitions/) — new CoreService method or MCP tool, new AgentService method, new transcript entry type, new defined agent, or a change to the worker's session behavior. Use when the user wants to extend the fleet's own code, not a target repo it manages.
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

# /fleet-feature — extend agent-fleet's own code

This is for changing this repo's own source, not for a session a worker runs
against a target repo. Code is the truth; `docs/ARCHITECTURE.md` gives the
topology and `docs/adr/` the reasons. Read the ADR a checklist names before
changing the shape it decided.

**The trust boundary, before touching anything**: `core` is the fleet's sole
Postgres-credential holder and the only component that calls the provisioner
(hub-and-spoke, `docs/adr/0020`). If a feature seems to want a second
component reaching Postgres, or a sidecar/worker reaching the provisioner,
route it through `core`'s `CoreService` instead.

## Adding a new agent-facing MCP tool

Agent-facing tools live in `sidecar/internal/mcpserver/server.go` and proxy to
`core` over gRPC. MCP is local only: the agent dials its own pod's sidecar
(`docs/adr/0048` §6).

1. Add the RPC to `proto/agentfleet/v1/core.proto`'s `CoreService`, run
   `buf generate` in `proto/`, and commit `proto/gen/go` and
   `dashboard/src/gen` (CI's drift check fails otherwise). The worker has no
   generated code; it speaks plain HTTP/JSON to its sidecar.
2. **Authorize it** in `core/internal/coreserver/auth.go`: put the method in
   exactly one of `callerSessionOf` (bound to the caller's own session — name
   the request field explicitly, never by field-name convention),
   `unboundMethods` or `provisionerMethods` (`docs/adr/0057`).
   `TestAuth_EveryMethodHasAnAuthorizationRule` fails until you do. Decide
   separately whether a definition pod may call it: `definitionMethods`
   (`docs/adr/0064`); unlisted means denied.
3. Implement the handler in `core/internal/coreserver/server.go`.
4. Add the client method to `sidecar/internal/coreclient/client.go`.
5. Register the tool in `sidecar/internal/mcpserver/server.go`'s `New()`
   (`s.AddTool(...)`).
6. Nothing to add to the worker's `allowedTools`: the
   `mcp__agent-fleet-sidecar__*` wildcard in `worker/src/session.ts` covers
   it, and `canUseTool` allows the fleet's own tools (`FLEET_OWN_TOOL`,
   `docs/adr/0053`). Two decisions remain yours:
   - A tool that acts outside the session (another pod, a URL, shared state)
     belongs in `PLAN_MODE_STILL_ASKS`, so `plan` mode still asks.
   - A definition session sees only the sidecar tools its definition lists
     (`sidecarTools`, bounded by `sidecarCeiling` in
     `core/internal/definitions/load.go`).

## Adding a new wrapper-facing (non-agent) call

Anything the worker's own code needs that the agent never calls goes through
the sidecar's local HTTP/JSON API, `sidecar/internal/localapi/server.go`
(`mux.HandleFunc` in `New()`), with the TS caller in
`worker/src/sidecarClient.ts`. Same proto, `auth.go`, `coreserver` and
`coreclient` steps as above.

## Adding a new AgentService method

`AgentService` (`proto/agentfleet/v1/agent.proto`, `docs/adr/0066`) is the API
members (console and islands, `:8080`) and registered clients (`:9091`) drive
definition sessions through. Handlers live in `core/internal/agentsvc/`.

1. Add the RPC to `agent.proto`, `buf generate`, commit the Go and
   `dashboard/src/gen` output (islands import the same generated client).
2. Add its row to `table` in `core/internal/agentsvc/authz.go`: which
   principals may call it at all (`member`, `client`). A method with no row
   is refused, and `TestAuthzTable_CoversEveryMethod` (which walks the
   proto's methods) fails.
3. Implement it on `*Server`. Until you do, the embedded
   `UnimplementedAgentServiceHandler` answers `Unimplemented` behind the
   table (that is the state of `Invoke`/`WaitForResult` today).
   Per-thread and per-definition decisions belong in the handler: resolve
   the thread through `s.thread` (visibility, `NotFound` for what the caller
   may not see) and the definition through `s.usable`; owner-only actions
   check `ownsOrAdmin` (`docs/adr/0067`).
4. A member call from an island carries `Principal.Origin`: member rights
   only, within the definitions whose `embed.origins` list it
   (`docs/adr/0068`). Never let an island reach `DashboardService`
   (`islands/src/imports.test.ts` guards the imports).

## Adding a new transcript entry type

1. Add the value to `TranscriptEntryType` in
   `proto/agentfleet/v1/transcript.proto`.
2. A new migration pair that replaces the `transcript.type` CHECK
   (`db/migrations/`, never an edit to an applied one — `docs/adr/0030`).
3. Map it both ways in every converter: `core/internal/coreserver/server.go`,
   `core/internal/dashboard/server.go`,
   `sidecar/internal/localapi/server.go`.
   `core/internal/coreserver/type_map_test.go` checks every enum value maps.
4. Give it a tier in `dashboard/src/components/SessionFeed.tsx`
   (`docs/dashboard-spec.md` §3): a new kind belongs in a tier, not a grey
   log line (`docs/adr/0042`).

## Adding a new defined agent

Definitions are reviewed config, not code (`docs/adr/0062`, pod profile
`0064`, credentials `0065`, modes and grants `0076`).

1. `definitions/<name>.yaml` (`kind: AgentDefinition`), usually
   `extends: workspace-base`. A child's top-level key **replaces** the
   parent's; nothing deep-merges. The ceiling is in
   `core/internal/definitions/load.go` (modes `auto`/`default`/`plan`, the
   tool and sidecar-tool ceilings, models, audience groups, resources).
2. A client that may open it: `definitions/clients/<name>.yaml`
   (`kind: AgentClient`), and the definition must name the client too
   (two-sided agreement).
3. `go test ./internal/definitions/` in `core/` runs
   `TestRepoDefinitions` (`repo_test.go`), the same validator core runs at
   sync. CI fails on any invalid non-abstract definition.
4. Its `workspace.repo` must be a row in `repos` (the GitHub App installed
   on it, `docs/adr/0074`/`0075`), and `credentials` key names must exist in
   Infisical project `agent-vault`, folder `/agent-vault`: a missing one
   fails the warm closed, naming it.
5. It goes live at the next **release tag**, not on merge: the provisioner
   reads `definitions/` at its own version (`FLEET_DEFINITIONS_REF`
   overrides). No definition warms while `MAX_DEFINITION_SESSIONS` is 0, and
   no member or client session while `MAX_EXTERNAL_SESSIONS` is 0
   (`core/internal/config/config.go`).

## Changing the worker's session behavior

Read `worker/src/session.ts`'s `runSession` in full first. It is **one
streaming-input `query()` session** spanning plan and implementation
(`docs/adr/0029`), resumed across pods through `RESUME_SESSION_ID`. The
load-bearing pieces:

- `InputQueue` feeds `streamInput()`.
- `canUseTool` reproduces the SDK's live permission prompt: it posts a
  `permission_request` and blocks until a human answers. Its two
  short-circuits (fleet-own tools; `auto` except `rm`/`sudo` and
  `ExitPlanMode`) are the fleet's only standing answers (`docs/adr/0053`).
  A definition session decides through `definitionDecision` instead
  (`docs/adr/0076`).
- The human-message consumer (`sidecar.streamHumanMessages`) handles
  `answer`, `abort`, `interrupt`, `permission_response` and
  `permission_mode` entries live.
- The `for await (const msg of q)` loop logs every SDK message
  (`logSdkMessage`).
- `FLEET_ASK_RULES` and `settings` are injected in flag scope, so a target
  repo's own settings cannot widen them (`docs/adr/0052`).

Never infer a decision from silence or round completion: a decision is a
structured `RespondToPermission` or `SetPermissionMode`. A new guardrail
defaults to unbounded/opt-in, not a fixed cap (`docs/adr/0008`). Which
skills the agent runs (`doubt-driven-development`, `architecture-interview`)
is its own judgment, loaded from packs (`packs/repo/skills/`); don't add a
flag to gate them (`docs/adr/0017`).

## Before committing

- `cd worker && bun run typecheck && bun run test` — the SDK/sidecar mocks in
  `worker/src/session.test.ts` are the load-bearing coverage for any
  `runSession` change.
- `cd core && go build ./... && go vet ./...`, same for `provisioner`/
  `sidecar` — plus `golangci-lint run` and `go test -race ./...` for anything
  non-trivial (`.github/workflows/go.yml` is the exact gate, including
  `-tags=integration` tests against a real Postgres).
- If `proto/` changed: `cd proto && buf lint && buf generate`, and commit the
  output.
- A Dockerfile change: every component builds from the repo root as context
  (`docker build -f worker/Dockerfile .`), because `proto/gen/go` is a local
  module resolved through `go.work`.
- Touched create, decision or warm paths: run `/dashboard-e2e`.
