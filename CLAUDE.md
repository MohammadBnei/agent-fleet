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

## Tech stack

| Layer | Tool |
|---|---|
| Runtime (`worker/` only) | Bun (`oven/bun:1-slim`), TypeScript, no build step — Bun runs `.ts` sources directly. The sole remaining JS runtime in the fleet — it's the only process hosting the Agent SDK |
| Worker agent runtime | `@anthropic-ai/claude-agent-sdk` (`query()`, not `claude -p` headless), model `claude-opus-4-8` |
| Runtime (`fleet-core/`, `e2e-provisioner/`) | Go, `go.work` workspace, `golangci-lint` |
| Discord | `discordgo` (Go, in `fleet-core/`) |
| Coordination | Postgres `planning_transcript` — pull/cursor reads via `fleet-core`'s MCP server, real idempotency-keyed dedup, not pub/sub (see `docs/adr/0013`) |
| MCP | `mark3labs/mcp-go` HTTP server (Go, `fleet-core/`/`e2e-provisioner/`), `@modelcontextprotocol/sdk` HTTP client (TS, `worker/`) |
| gRPC/proto | `buf`-managed `.proto` schema (`proto/`) — the one real internal gRPC call in the fleet (`fleet-core` → `e2e-provisioner`), plus the dashboard's ConnectRPC API (`fleet-core` ↔ `dashboard/`, `connect-go`/`connect-web`, see `adr/0015`) |
| Kubernetes client | `client-go` (`e2e-provisioner/`) |
| Database | `pg` (`worker/`), `jackc/pgx/v5` (Go services) against `agentfleetdb` (Pigsty-managed Postgres, shared with the rest of the homelab) |
| Deploy | Docker (`worker/Dockerfile`, `fleet-core/Dockerfile`, `e2e-provisioner/Dockerfile`), two-source ArgoCD Application (chart from `infra-bootstrap`, values from `k8s/` here) |
| CI/CD | GitHub Actions: `docker.yml` (build/push/deploy), `go.yml` (Go vet/lint/test + buf lint/breaking/drift), `release.yml` (`release-it`, conventional-changelog) |

## Directory map

| Path | What |
|---|---|
| `docs/ARCHITECTURE.md` | Canonical topology + current features (the WHAT) |
| `docs/DECISIONS.md` | Canonical settled decisions, forbidden patterns (the WHY, short form) |
| `docs/adr/` | One Architecture Decision Record per real decision |
| `fleet-core/` | Go — Discord ingress + planning-transcript coordination + Loki queries + the web dashboard's ConnectRPC API and static SPA, one binary, no cluster RBAC |
| `dashboard/` | React + Vite + TypeScript + Tailwind/DaisyUI SPA, talks to `fleet-core` via a generated ConnectRPC client, built into `fleet-core`'s binary — not deployed on its own |
| `worker/` | The Claude Code worker (TS/Bun) — polls tasks, runs planning + implementation phases |
| `proto/` | buf-managed `.proto` schema shared by `fleet-core`/`e2e-provisioner` (Go codegen) and `worker`/`dashboard` (TS codegen) |
| `db/schema.sql` | Shared `tasks`/`knowledge_journal`/`planning_transcript`/`e2e_sessions` tables (`agentfleetdb`) |
| `k8s/` | Helm values consumed by `infra-bootstrap`'s two-source ArgoCD Applications |
| `e2e-provisioner/` | Go — the only fleet component with cluster RBAC, provisions on-demand e2e pods |

## Locked decisions (condensed — full detail + rationale in `docs/DECISIONS.md` and `docs/adr/`)

- Planning coordination is a durable Postgres table
  (`planning_transcript`), read via a pull/cursor API, never a bare
  streaming-watch RPC without a resume cursor — a dropped message during a
  plan-mode approval gate isn't recoverable (see `docs/adr/0013`,
  successor to the original Redis-list-over-pubsub decision).
- No orchestration framework (Hermes/OpenClaw rejected) — proposer and
  critic are independent Agent SDK `query()` sessions coordinating only
  through the shared transcript.
- One persistent worker pod per target repo, polling Postgres; git
  worktree isolation happens per-task inside the pod, not at
  pod-lifecycle level.
- Write/edit tools unlock only after Mohammad's **explicit** approval —
  never inferred from silence or round completion.
- Every result is a PR. No auto-merge, ever.
- One git worktree per task — never a shared writable repo checkout across
  concurrent tasks.
- Git commit identity is derived live from the authenticated bot GitHub
  account (`gh api user`), never hardcoded.
- Claude Code auths via subscription OAuth (`CLAUDE_CODE_OAUTH_TOKEN`), not
  a metered API key — no cost cap by design.
- Secrets only via Infisical (project `agent-fleet-nygh`) — never
  committed.

## Forbidden patterns (quick check — full list + reasons in `docs/DECISIONS.md`)

Auto-merge · shared writable repo PVC across tasks · inferring approval
from silence · hardcoded git commit identity · committing Discord/GitHub/
Anthropic tokens · a bespoke per-app Helm chart (reuse
`infra-bootstrap/gitops/platform/common-app-chart`) · fleet-managed
`infra-bootstrap` cluster ops (kubespray/ansible/pigsty — that's the
parent repo's own `CLAUDE.md` boundary, explicitly blocked here too).

## Current targets

`dream-analyst`, `vos-monolith` — real repos, each with its own persistent
worker pod (`k8s/dream-analyst-worker.yaml`, `k8s/vos-monolith-worker.yaml`).

## Workflow rules

- All changes via feature branch + PR. No direct push to `main`.
- Secrets never committed; always fetched from Infisical at run time.
- Commit messages follow Conventional Commits — `release.yml` runs
  `release-it` off them to bump version/CHANGELOG on every push to `main`.
- `worker/` has `bun:test` coverage (`bun run test`) for coordination logic
  that's cheap to mock — the SDK's `query()` and the fleet-core MCP client
  — run in CI ahead of the Docker build; also has `bun run typecheck`.
- `fleet-core/` and `e2e-provisioner/` have `go test` coverage: table-driven
  unit tests, `client-go`'s fake clientset for k8s manifest shape, `bufconn`
  for gRPC roundtrips, and `testcontainers-go`-backed integration tests
  (gated `-tags=integration`) against a real Postgres — see `.github/
  workflows/go.yml`. `golangci-lint` runs alongside.

## Skills

- `/fleet-ops` — onboard a new target repo/worker, walk the release/deploy
  pipeline, inspect the live task queue.
- `/fleet-feature` — checklist for adding functionality to `fleet-core/`,
  `worker/`, or `e2e-provisioner/` (new MCP tool, new slash command, new
  task status).
- `/fleet-debug` — diagnose a stuck or failed task (task/journal state,
  worker logs, the `planning_transcript` table, known failure modes).
