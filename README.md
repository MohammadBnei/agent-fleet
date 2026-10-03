# agent-fleet

A self-hosted fleet of Claude Code workers: each session owns one piece of work
end-to-end (plan → code → tests → docs), driven from a web dashboard, running
on `ukubi-cluster`.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that's where the cluster itself (Kubernetes, ArgoCD,
ingress, secrets backend) is provisioned and documented. This repo holds the
fleet's own source (`core`, `provisioner`, `sidecar`, `worker`) and deploy
config (`k8s/`).

## Status

See **[`CLAUDE.md`](./CLAUDE.md)** for orientation and
**[`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md)** for the canonical
topology and current features. The shape since v3.0.0 is
[`docs/adr/0048`](./docs/adr/0048-one-session-one-pod-one-shared-home.md):
one session, one pod, one shared home. A session is a database row; creating
one starts nothing. The first message (or an explicit Warm, opening a
proposal, or a prompt from another session) makes `core` command
`provisioner` to start a single-shot worker Job for that session, up to `MAX_IN_FLIGHT_TASKS` (default 5) live pods
across all repos. Each session has its own node-local volume holding a real
clone, which borrows objects from a shared clone cache through git alternates.
There is no queue, no status enum and no worktree; the agent creates its own
branch. Idle pods are torn down and the session resumes in a fresh pod on the
next message. Machine-initiated work (alerts, schedules) only files a
proposal; a human opens it.

- `core/` — Go: the dashboard's ConnectRPC API + static SPA, behind
  authentik OIDC terminated in `core` itself
  ([`docs/adr/0056`](./docs/adr/0056-the-console-gate-is-oidc-in-core.md));
  its own gRPC server (`CoreService`), where every call is authenticated by
  the session's lease
  ([`docs/adr/0057`](./docs/adr/0057-coreservice-authenticates-with-the-session-lease.md));
  the Postgres `transcript` that coordinates sessions; a 60s reconcile loop
  that tracks pod state and tears down idle pods; alert and schedule intake
  that files proposals; Loki/Prometheus queries; and outbound-only Discord
  notifications that link to the dashboard. The fleet's sole holder of
  Postgres credentials; needs zero cluster RBAC.
- `provisioner/` — Go, `client-go`: the only fleet component with
  Kubernetes RBAC. Creates each session's worker as a `batch/v1.Job` with a
  per-session PVC, keeps the shared clone cache on the shared PVC, and
  publishes preview routes a session asks for.
- `sidecar/` — Go: a second container in every worker pod. Hosts the local
  MCP server the Agent SDK session talks to (questions to the human, journal,
  logs, prompting other sessions), plus a local HTTP API for the worker
  (including the live feed of human messages). All of it goes through one
  outbound gRPC connection to `core`.
- `worker/` — TS/Bun, the only remaining JS runtime (sole host of
  `@anthropic-ai/claude-agent-sdk`). Single-shot: runs one streaming-input
  Agent SDK session per pod, resumable in the next pod. Permission prompts
  are reproduced through `canUseTool` and answered by a human in the
  dashboard. The agent itself commits, pushes and opens a PR via `gh`.
- `executor/` — Go: the cluster-access shim installed as `kubectl` in a
  cluster-access session; the fleet's only process holding cluster RBAC on
  an agent's behalf
  ([`docs/adr/0037`](./docs/adr/0037-thot-is-a-worker-task.md)).
- `dashboard/` — React SPA, built into the `core` binary.
- `proto/` — buf-managed `.proto` schema for `CoreService`/
  `ProvisionerService`/`DashboardService` — the only inter-process
  protocol in the fleet (MCP is local-only, agent ↔ its own pod's
  sidecar).
- `db/migrations/` — sole source of truth (golang-migrate, see
  `docs/adr/0030`) for `sessions`, `proposals`, `transcript`, `repos`,
  `schedules`, `scheduled_audits`, `prompt_snippets` and
  `knowledge_journal`, in the fleet-wide `agentfleetdb` Postgres database
  (Pigsty).

How this compares with another self-hosted fleet built for the same reason:
[`docs/comparison-k-k1-agent-fleet.md`](./docs/comparison-k-k1-agent-fleet.md).

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
- Worker/sidecar pods are ephemeral, spawned on demand by `provisioner`,
  not persistent per-repo deployments.
  `core` deploys as a normal gitops app (`infra-bootstrap`'s `/add-app`
  pattern, reusing `gitops/platform/common-app-chart`); `provisioner`'s
  manifests live in this repo (`k8s/provisioner/`) and register as a
  standalone Application in `infra-bootstrap`.
- Per `infra-bootstrap`'s own `CLAUDE.md`, this fleet does **not** manage
  `infra-bootstrap`'s own cluster ops (kubespray/ansible/pigsty) — that's
  explicitly blocked until revisited (see `docs/DECISIONS.md`).
