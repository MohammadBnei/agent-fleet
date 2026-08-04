# DECISIONS

Settled-decisions log for `agent-fleet`. This is the WHY: rationale for
choices that were never really in question, plus a quick-reference list of
things not to propose. It is **not** a spec doc — topology and current
features live in [`ARCHITECTURE.md`](ARCHITECTURE.md). Full alternative-
weighing for anything that had real competing options lives in
[`adr/`](adr/README.md), one file per decision, independently trackable by
status.

Any doc, code, comment, or memory that contradicts this file or an
`Accepted` ADR is overridden by them until the file/ADR itself is updated.

## Reading order (for AI agents)

1. This file (`DECISIONS.md`) — hard prerequisite.
2. [`ARCHITECTURE.md`](ARCHITECTURE.md) for topology/current features.
3. [`adr/`](adr/README.md) for the reasoning behind any specific decision.
4. Then the source itself (`worker/src/`, `fleet-core/internal/`,
   `e2e-provisioner/internal/`).

---

## 1. Locked decisions (no dedicated ADR — never really in question)

- **Bun is the sole JS runtime for `worker/`** — the only remaining TS/Bun
  component (superseded 2026-08-03 by [`adr/0013`](adr/0013-go-fleet-core-and-e2e-provisioner-rewrite.md)
  for `bot/`/`mcp-redis/`, which no longer exist; `fleet-core`/
  `e2e-provisioner` are Go). No build step for `worker/`'s TypeScript
  sources — runs directly. No npm/pnpm/yarn workspace tooling; `worker/`
  keeps its own `package.json`/`bun.lock`, and each Go module keeps its own
  `go.mod` under the shared `go.work`.
- **Git auth goes through the `gh` CLI**, not a hand-rolled token-in-header
  client — `gh auth setup-git` wires `GH_TOKEN` into git's credential
  helper once, and `gh pr create`/`gh api` reuse the same token.
- **`knowledge_journal` is append-only**, not a mutable shared doc — avoids
  write-conflict issues a shared mutable record would hit across
  concurrent worker pods (dream-analyst-worker and vos-monolith-worker
  write to it independently).
- **One repo, one version/CHANGELOG.** `release.yml` runs from the repo
  root, not per-package — the fleet ships as one unit even though
  `worker/`, `fleet-core/`, `e2e-provisioner/` are deployed as separate
  images.
- **Workflow discipline:** all changes via feature branch + PR, no direct
  push to `main`; secrets only via Infisical, fetched at run time, never
  committed.

## 2. Forbidden patterns (quick check — full list + reasons in `adr/`)

- **Auto-merge, ever.** Every task result is a PR; a human merges.
- **A shared writable repo PVC across tasks.** One git worktree per task,
  always — see `adr/0003`.
- **Inferring approval from silence or round completion.** Only an
  explicit `/approve` (or its word-match fallback) unlocks write/edit —
  see `adr/0005`.
- **Hardcoding git commit identity.** Always derived live from the
  authenticated bot GitHub account — see `adr/0006`.
- **Committing Discord/GitHub/Anthropic tokens** to this repo or any
  target repo, in code, manifests, or CI config.
- **A bespoke per-app Helm chart.** Always reuse
  `infra-bootstrap/gitops/platform/common-app-chart`.
- **A bare streaming-watch RPC without a durable resume cursor for the
  planning transcript.** Pull/cursor reads only — see `adr/0013`
  (successor to the same message-loss concern `adr/0001` raised about
  Redis pub/sub, now against a gRPC/Postgres backend instead of Redis).
- **An orchestration framework (Hermes, OpenClaw, or similar).** A single
  Agent SDK planning session, using real Claude Code skills
  (doubt-driven-development, architecture-interview) for structured
  review/elicitation instead of a second independent session or an
  external orchestrator — see `adr/0002` (superseded) and `adr/0017`.
- **Fleet-managed `infra-bootstrap` cluster ops.** That repo's own
  `CLAUDE.md` states a human runs kubespray/ansible/pigsty personally;
  this fleet does not touch that, full stop, until that decision is
  explicitly revisited in `infra-bootstrap` itself.

Don't propose any of these without an explicit greenlight from Mohammad,
even as a "better alternative" — each one was tried, considered, or hit a
live incident that's recorded in `adr/`.
