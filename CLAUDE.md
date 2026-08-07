# agent-fleet

A self-hosted fleet of Claude Code workers: each owns one feature end-to-end
(plan → code → tests → docs), triggered from Discord, running on
`ukubi-cluster`.

**`docs/ARCHITECTURE.md` is canonical for topology/current features;
`docs/DECISIONS.md` + `docs/adr/` are canonical for decisions/rationale.**
This file is a condensed summary for quick orientation — if they disagree,
`docs/ARCHITECTURE.md` wins for specs, `docs/DECISIONS.md`/`docs/adr/` win
for decisions. When in doubt, open those files.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that repo owns the cluster (Kubernetes, ArgoCD, ingress,
secrets backend); this repo owns the fleet's own source and deploy config.

As of `docs/adr/0019`–`0021`: `fleet-core` → `core`, `e2e-provisioner` →
`provisioner`, plus a new `sidecar` component and a rewritten single-shot
`worker/`. There is no per-repo persistent worker pod anymore — see below.
As of `docs/adr/0029`: a **session** (not a task) is the durable unit — a
worker pod is ephemeral compute attached to a session on demand ("warm"),
and `canUseTool` reproduces the Agent SDK's own live permission-prompt
tiers instead of a fleet-imposed Write/Edit approval gate.

## Tech stack

| Layer | Tool |
|---|---|
| Runtime (`worker/` only) | Bun (`oven/bun:1-slim`), TypeScript, no build step — Bun runs `.ts` sources directly. The sole remaining JS runtime in the fleet — it's the only process hosting the Agent SDK |
| Worker agent runtime | `@anthropic-ai/claude-agent-sdk` `query()` in **streaming-input mode** (not `claude -p` headless, not the plain-string form) — one continuous session spans planning and implementation, starts in `"default"` permission mode (CLI parity), model `claude-opus-4-8` (see `docs/adr/0021`/`0029`) |
| Runtime (`core/`, `provisioner/`, `sidecar/`) | Go, `go.work` workspace, `golangci-lint` |
| Discord | `discordgo` (Go, in `core/`) — secondary/notification channel, not primary (`docs/adr/0029`) |
| Coordination | Postgres `transcript` (renamed from `planning_transcript`) — pull/cursor reads via `core`'s gRPC `CoreService`, real idempotency-keyed dedup, not pub/sub (see `docs/adr/0013`/`0020`) |
| MCP | `mark3labs/mcp-go` HTTP server, **local-only** (Go, `sidecar/` — one per worker pod, agent connects over `localhost`). No inter-pod MCP anywhere in the fleet as of `docs/adr/0020` point 6 |
| gRPC/proto | `buf`-managed `.proto` schema (`proto/`) — the only inter-process/inter-pod protocol in the fleet: `CoreService` (core's server — sidecar + provisioner call it), `ProvisionerService` (provisioner's server — only core calls it), plus the dashboard's ConnectRPC API (`core` ↔ `dashboard/`, `connect-go`/`connect-web`, see `adr/0015`) |
| Kubernetes client | `client-go` (`provisioner/` — the only fleet component with cluster RBAC) |
| Database | `jackc/pgx/v5` (`core/` only — the fleet's **sole** `AGENTFLEET_DB_*` credential holder, `docs/adr/0020` point 1). `worker/`/`sidecar/`/`provisioner/` hold zero DB credentials |
| Deploy | Docker (`worker/Dockerfile`, `core/Dockerfile`, `provisioner/Dockerfile`, `sidecar/Dockerfile`), `core` via a two-source ArgoCD Application (chart from `infra-bootstrap`, values from `k8s/core.yaml` here); `provisioner` as a standalone plain-manifest Application (`k8s/provisioner/` here, RBAC `common-app-chart` can't express) |
| CI/CD | GitHub Actions: `docker.yml` (build/push/deploy), `go.yml` (Go vet/lint/test + buf lint/breaking/drift), `release.yml` (`release-it`, conventional-changelog) |

## Directory map

| Path | What |
|---|---|
| `docs/ARCHITECTURE.md` | Canonical topology + current features (the WHAT) |
| `docs/DECISIONS.md` | Canonical settled decisions, forbidden patterns (the WHY, short form) |
| `docs/adr/` | One Architecture Decision Record per real decision |
| `core/` | Go — Discord ingress + dispatch loop (claims tasks, commands the provisioner; also sweeps stop-grace and idle-timeout, both `pod_phase`-gated) + `CoreService` gRPC server + transcript coordination + Loki queries + the web dashboard's ConnectRPC API and static SPA, one binary, **no cluster RBAC**, sole Postgres-credential holder |
| `provisioner/` | Go — the **only** fleet component with cluster RBAC. Owns all pod creation (worker pods **and** e2e-preview pods) and the entire git lifecycle (clone/fetch/worktree add+remove) on the one shared workspace PVC. Zero DB credentials — Kubernetes itself is its source of truth |
| `sidecar/` | Go — new second container in every worker pod. Local MCP server (agent-facing) + local plain HTTP/JSON API (wrapper-facing, including the live human-message SSE feed) + an independent telemetry loop, all funneled through one outbound gRPC connection to `core` |
| `dashboard/` | React + Vite + TypeScript + Tailwind/DaisyUI SPA, talks to `core` via a generated ConnectRPC client, built into `core`'s binary — not deployed on its own |
| `worker/` | The Claude Code worker (TS/Bun; `src/session.ts`, renamed from `planning.ts`) — **single-shot**: one pod per warm, one continuous streaming-input session spanning planning+implementation (resumable via `RESUME_SESSION_ID`), then exits. Talks only to its own pod's `localhost` sidecar |
| `proto/` | buf-managed `.proto` schema: `CoreService`, `ProvisionerService`, `DashboardService` — shared by `core`/`provisioner` (Go codegen) and `worker`/`dashboard` (TS codegen) |
| `db/schema.sql` | Shared `tasks`/`knowledge_journal`/`transcript` tables (`agentfleetdb`). `e2e_sessions` still exists in the schema but is dead code — see `docs/ARCHITECTURE.md` §6 |
| `k8s/` | `core.yaml` (Helm values, `common-app-chart`) + `provisioner/` (standalone plain manifests: Deployment/Service/ServiceAccount/Role/InfisicalSecret/NetworkPolicy/PVC) |

## Locked decisions (condensed — full detail + rationale in `docs/DECISIONS.md` and `docs/adr/`)

- Session coordination is a durable Postgres table (`transcript`, renamed
  from `planning_transcript`), read via a pull/cursor API, never a bare
  streaming-watch RPC without a resume cursor — a dropped message during a
  live permission decision isn't recoverable (see `docs/adr/0013`,
  successor to the original Redis-list-over-pubsub decision).
- No orchestration framework (Hermes/OpenClaw rejected) — a single Agent
  SDK session, using real Claude Code skills (doubt-driven-development,
  architecture-interview) for review/elicitation instead of a second
  independent session — see `docs/adr/0017`.
- **One pod per warm, single-shot, spawned on demand by the provisioner —
  not one persistent pod per target repo, and not tied to a session's
  entire lifetime.** A pod is ephemeral compute attached to a session
  on-demand (`Warm`, or a fresh dispatch), torn down on Stop or idle
  timeout, resumable later via the session's saved `session_id`
  (`docs/adr/0029`). Fleet-wide concurrency (default cap 5, reinterpreted
  as "max warm pods"), not one-task-per-repo. Git worktree isolation
  happens on the one shared PVC, keyed by task ID, before the pod even
  exists (`docs/adr/0019`).
- **Postgres access is fully centralized in `core`.** No other
  component — provisioner, worker, or sidecar — ever holds
  `AGENTFLEET_DB_*` credentials (`docs/adr/0020` point 1).
- **`core` commands, the provisioner executes — never the reverse.** The
  provisioner never claims tasks or decides to spawn a pod on its own
  (`docs/adr/0020` point 2).
- **Hub-and-spoke: nothing talks to the provisioner except `core`.**
  Includes e2e-pod requests — proxied agent → sidecar (MCP) → `core`
  (gRPC) → provisioner (gRPC), not a direct path (`docs/adr/0020` point 4).
- **MCP is purely local** (agent ↔ its own pod's sidecar). **gRPC is the
  only inter-process/inter-pod protocol anywhere in the fleet**
  (`docs/adr/0020` point 6).
- **No prospective Write/Edit gate — `canUseTool` reproduces the Agent
  SDK's own live permission prompt instead** (`docs/adr/0029`, supersedes
  `docs/adr/0021`'s `allowedTools`-absent + in-memory-`approved`-flag
  mechanism). Sessions start in `"default"` mode (CLI parity); the SDK's
  own `permissionMode` decides when `canUseTool` gets invoked, and
  `canUseTool` just asks a human and blocks — never inferred from silence
  or round completion. A permission decision is always a real, structured
  `RespondToPermission` (or `SetPermissionMode`) call. `/approve` no
  longer exists.
- Every result is a PR. No auto-merge, ever.
- One git worktree per task — never a shared writable repo checkout across
  concurrent tasks.
- Git commit identity is derived live from the authenticated bot GitHub
  account (`gh api user`), never hardcoded — done independently by both
  the provisioner (clone/fetch) and the worker (push/PR), since they're
  separate pods that don't share `$HOME`.
- Claude Code auths via subscription OAuth (`CLAUDE_CODE_OAUTH_TOKEN`), not
  a metered API key — no cost cap by design.
- Secrets only via Infisical (project `agent-fleet-nygh`) — never
  committed.

## Forbidden patterns (quick check — full list + reasons in `docs/DECISIONS.md`)

Auto-merge · shared writable repo PVC across tasks · inferring a permission
decision from silence · hardcoded git commit identity · committing Discord/GitHub/
Anthropic tokens · a bespoke per-app Helm chart (reuse
`infra-bootstrap/gitops/platform/common-app-chart`) · any component other
than `core` holding `AGENTFLEET_DB_*` credentials · a worker/sidecar
talking to the provisioner directly (must route through `core`) · a direct
inter-pod MCP connection (MCP is local-only; gRPC is the only inter-pod
protocol) · fleet-managed `infra-bootstrap` cluster ops (kubespray/ansible/
pigsty — that's the parent repo's own `CLAUDE.md` boundary, explicitly
blocked here too).

## Current targets

`dream-analyst`, `vos-monolith`, `agent-fleet` — real repos, seeded into
the dashboard-editable `repos` table (`docs/adr/0028`) — no redeploy
needed to add/edit one. No per-repo Deployment/PVC — onboarding a new repo
is a "manage repos" entry in the dashboard, not new k8s manifests.

## Workflow rules

- All changes via feature branch + PR. No direct push to `main`.
- Secrets never committed; always fetched from Infisical at run time.
- Commit messages follow Conventional Commits — `release.yml` runs
  `release-it` off them to bump version/CHANGELOG on every push to `main`.
- `worker/` has `bun:test` coverage (`bun run test`) for coordination logic
  that's cheap to mock — the SDK's streaming-input `query()` and the
  sidecar's local HTTP client — run in CI ahead of the Docker build; also
  has `bun run typecheck`.
- `core/`, `provisioner/`, and `sidecar/` have `go test` coverage:
  table-driven unit tests, `client-go`'s fake clientset for k8s manifest
  shape, `bufconn` for gRPC roundtrips, and `testcontainers-go`-backed
  integration tests (gated `-tags=integration`) against a real Postgres —
  see `.github/workflows/go.yml`. `golangci-lint` runs alongside.

## Skills

- `/fleet-ops` — onboard a new target repo, walk the release/deploy
  pipeline, inspect the live task queue.
- `/fleet-feature` — checklist for adding functionality to `core/`,
  `provisioner/`, `sidecar/`, or `worker/` (new gRPC method, new MCP tool,
  new slash command, new task status).
- `/fleet-debug` — diagnose a stuck or failed task (task/journal state,
  worker/sidecar/provisioner logs, the `transcript` table, known
  failure modes).
- `/dashboard-e2e` — spin up the minimal local stack (throwaway Postgres +
  `core` + dashboard dev server) to Playwright-test the dashboard UI, then
  tear it all down.
