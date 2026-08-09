# agent-fleet

A self-hosted fleet of Claude Code workers: each owns one feature end-to-end
(plan → code → tests → docs), triggered from Discord, running on
`ukubi-cluster`.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that's where the cluster itself (Kubernetes, ArgoCD,
ingress, secrets backend) is provisioned and documented. This repo holds the
fleet's own source (`core`, `provisioner`, `sidecar`, `worker`) and deploy
config (`k8s/`).

## Status

See **[`CLAUDE.md`](./CLAUDE.md)** for orientation and
**[`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md)** for the canonical
topology and current features — the golden path (one Discord message → one
on-demand Claude Code worker pod → one PR) is live. Task dispatch is
fleet-wide, not one-task-per-repo: `core` claims a pending task and commands
`provisioner` to spin up a single-shot, two-container worker pod (worker +
sidecar) with a git worktree already prepared on a shared PVC, up to
`MAX_IN_FLIGHT_TASKS` concurrent tasks across any known repo. See
[`docs/adr/0019`](./docs/adr/0019-shared-pvc-and-unified-provisioner.md)
for why this replaced the original one-persistent-pod-per-repo design.

- `core/` — Go: Discord ingress (`/task`/`/approve`/`/stop`/`/e2e-kill`,
  legacy `!task repo: desc`), the dispatch loop that claims tasks and
  commands the provisioner, its own gRPC server (`CoreService`), the
  planning-transcript coordination (Postgres), Loki log queries (agents can
  view logs from all fleet components and deployed apps via `view_logs` MCP
  tool for self-debugging), and the web dashboard's ConnectRPC API + static
  SPA. The fleet's sole holder of Postgres credentials; needs zero cluster
  RBAC.
- `provisioner/` — Go, `client-go`: the only fleet component with
  Kubernetes RBAC. Owns all pod creation — worker sessions as a
  `batch/v1.Job`, on-demand e2e-preview pods as a bare `Pod` — and the
  entire git lifecycle (clone/fetch/worktree add, reuse-not-wipe, plus a
  periodic sweep) on one shared `ReadWriteMany` PVC.
- `sidecar/` — Go: a second container in every worker pod. Hosts a local
  MCP server the Agent SDK session talks to (including `view_logs` for
  agent self-debugging), plus a local plain API for the worker's own
  control-flow (heartbeat, status, journal, and the live human-message feed
  that lets a Discord/dashboard reply reach the running session mid-task).
- `worker/` — TS/Bun, the only remaining JS runtime (sole host of
  `@anthropic-ai/claude-agent-sdk`). Single-shot: handed one task at pod
  creation, runs one continuous streaming-input Agent SDK session with no
  fleet-imposed phase boundary — the agent itself commits, pushes, and
  opens a PR via `gh` from inside the session, and the wrapper verifies
  the PR actually exists before declaring done.
- `proto/` — buf-managed `.proto` schema for `CoreService`/
  `ProvisionerService`/`DashboardService` — the only inter-process
  protocol in the fleet (MCP is local-only, agent ↔ its own pod's
  sidecar).
- `db/migrations/` — sole source of truth (golang-migrate, see
  `docs/adr/0030`) for the shared `tasks` queue, append-only
  `knowledge_journal`, and `transcript`, in the fleet-wide `agentfleetdb`
  Postgres database (Pigsty).

Deployment config lives in `k8s/` in this repo: `core.yaml` (Helm values, a
two-source ArgoCD Application — chart from `infra-bootstrap`, values from
here) and `provisioner/` (standalone plain manifests, since it needs RBAC
`common-app-chart` can't express).

Only the Application/ApplicationSet registration itself lives in
`infra-bootstrap`'s `gitops/` — see that repo's `gitops/README.md`.

## Relationship to `infra-bootstrap`

- The cluster (`ukubi-cluster`), GitOps (`gitops/`), and secrets backend
  (Infisical) are all owned by `infra-bootstrap` — this repo consumes
  them, it doesn't redefine them.
- Worker/sidecar pods are ephemeral, spawned on demand by `provisioner`
  (see `docs/ARCHITECTURE.md` §5), not persistent per-repo deployments.
  `core` deploys as a normal gitops app (`infra-bootstrap`'s `/add-app`
  pattern, reusing `gitops/platform/common-app-chart`); `provisioner`'s
  manifests live in this repo (`k8s/provisioner/`) and register as a
  standalone Application in `infra-bootstrap`.
- Per `infra-bootstrap`'s own `CLAUDE.md`, this fleet does **not** manage
  `infra-bootstrap`'s own cluster ops (kubespray/ansible/pigsty) — that's
  explicitly blocked until revisited (see `docs/DECISIONS.md`).
