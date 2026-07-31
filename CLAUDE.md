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
| Runtime | Bun (`oven/bun:1-slim`), TypeScript throughout, no build step — Bun runs `.ts` sources directly |
| Worker agent runtime | `@anthropic-ai/claude-agent-sdk` (`query()`, not `claude -p` headless), model `claude-opus-4-8` |
| Discord | `discord.js` v14 |
| Coordination | `ioredis` — durable RPUSH/LRANGE list, not pub/sub (see `docs/adr/`) |
| MCP | `@modelcontextprotocol/sdk` stdio server (`mcp-redis/`) |
| Database | `pg` against `agentfleetdb` (Pigsty-managed Postgres, shared with the rest of the homelab) |
| Deploy | Docker (`worker/Dockerfile`, `bot/Dockerfile`), two-source ArgoCD Application (chart from `infra-bootstrap`, values from `k8s/` here) |
| CI/CD | GitHub Actions: `docker.yml` (build/push/Trivy-scan/deploy), `release.yml` (`release-it`, conventional-changelog) |

## Directory map

| Path | What |
|---|---|
| `docs/ARCHITECTURE.md` | Canonical topology + current features (the WHAT) |
| `docs/DECISIONS.md` | Canonical settled decisions, forbidden patterns (the WHY, short form) |
| `docs/adr/` | One Architecture Decision Record per real decision |
| `bot/` | Discord bot — trigger ingress, task queueing, thread-reply relay |
| `worker/` | The Claude Code worker — polls tasks, runs planning + implementation phases |
| `mcp-redis/` | Stdio MCP server wrapping the shared Redis planning transcript |
| `db/schema.sql` | Shared `tasks` queue + `knowledge_journal` table (`agentfleetdb`) |
| `k8s/` | Helm values consumed by `infra-bootstrap`'s two-source ArgoCD Applications |

## Locked decisions (condensed — full detail + rationale in `docs/DECISIONS.md` and `docs/adr/`)

- Planning coordination is a durable Redis list (RPUSH + polled LRANGE),
  never pub/sub — a dropped message during a plan-mode approval gate isn't
  recoverable.
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
- No test framework exists for `bot/`/`worker`/`mcp-redis` — the golden
  path itself is the test (see `docs/ARCHITECTURE.md`). Each package does
  have `bun run typecheck`.

## Skills

- `/fleet-ops` — onboard a new target repo/worker, walk the release/deploy
  pipeline, inspect the live task queue.
- `/fleet-feature` — checklist for adding functionality to `bot/`,
  `worker/`, or `mcp-redis/` (new MCP tool, new slash command, new task
  status).
- `/fleet-debug` — diagnose a stuck or failed task (task/journal state,
  worker logs, the Redis transcript, known failure modes).
