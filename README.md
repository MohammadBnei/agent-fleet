# agent-fleet

A self-hosted fleet of Claude Code workers: each owns one feature end-to-end
(plan → code → tests → docs), triggered from Discord, running on
`ukubi-cluster`.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that's where the cluster itself (Kubernetes, ArgoCD,
ingress, secrets backend) is provisioned and documented. This repo holds the
fleet's own source (Discord bot, worker, MCP server) and deploy config
(`k8s/` Helm values).

## Status

See **[`CLAUDE.md`](./CLAUDE.md)** for orientation and
**[`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md)** for the canonical
topology and current features — the golden path (one Discord message → one
isolated Claude Code worker → one PR) is live: one persistent worker pod
per target repo (dream-analyst, vos-monolith), with git-worktree-per-task
isolation happening inside that pod rather than at the pod-lifecycle level.
See [`docs/adr/0003`](./docs/adr/0003-persistent-worker-pod-per-repo.md)
for why that revises the original "one Kubernetes Job per task" design.

- `bot/` — Discord bot: watches a trigger channel for `/task` (or the legacy
  `!task <repo>: <description>` free-text fallback), opens a Discord thread,
  inserts a row into the shared Postgres `tasks` table, and relays
  subsequent thread replies into the task's planning transcript (Redis) so
  the proposer/critic/human conversation is visible to all three.
- `worker/` — one image, deployed twice (once per target repo). Polls
  Postgres for its repo's pending tasks, creates a git worktree per task, runs
  a proposer + critic Agent SDK session (`claude-opus-4-8`) that debate the
  plan over the shared Redis transcript — read/bash-only until Mohammad
  replies "approved" in the thread — then resumes the **same** proposer
  session with write/edit unlocked to implement, test, commit, push, and open
  a PR via `gh`.
- `mcp-redis/` — stdio MCP server wrapping the shared planning transcript as a
  durable Redis list (not pub/sub — a list can't drop a message published
  while no one's subscribed).
- `db/schema.sql` — the shared `tasks` queue + an append-only
  `knowledge_journal` table, both in the fleet-wide `agentfleetdb` Postgres
  database (Pigsty).

Deployment config (Helm values) lives in `k8s/` in this repo — a two-source
ArgoCD Application (chart from `infra-bootstrap`, values from here, see
`infra-bootstrap`'s `gitops/apps/registry.yaml`) so `docker.yml`'s deploy job
can bump the pinned image tag on release without a cross-repo commit.

Only the Application/ApplicationSet registration itself lives in
`infra-bootstrap`'s `gitops/` — see that repo's `gitops/README.md`.

## Relationship to `infra-bootstrap`

- The cluster (`ukubi-cluster`), GitOps (`gitops/`), and secrets backend
  (External Secrets + SOPS/age) are all owned by `infra-bootstrap` — this
  repo consumes them, it doesn't redefine them.
- Workers run as persistent Kubernetes pods on `ukubi-cluster` (one per
  target repo, see `docs/ARCHITECTURE.md` §5); the Discord bot deploys as a
  normal gitops app, following `infra-bootstrap`'s `/add-app` pattern
  (reusing `gitops/platform/common-app-chart`).
- Per `infra-bootstrap`'s own `CLAUDE.md`, this fleet does **not** manage
  `infra-bootstrap`'s own cluster ops (kubespray/ansible/pigsty) — that's
  explicitly blocked until revisited (see `docs/DECISIONS.md`).
