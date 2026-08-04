# Architecture Decision Records

One file per real decision — a competing alternative existed, a choice was
reversed, or a live incident forced a change. Format: Status / Date /
Context / Decision / Consequences. Status is one of `Proposed`, `Accepted`,
`Rejected`, `Superseded`.

Decisions with no real alternative ever on the table live in
[`../DECISIONS.md`](../DECISIONS.md) §1 instead, not here.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-redis-durable-list-over-pubsub.md) | Durable Redis list over pub/sub for the planning transcript | Superseded by [0013](0013-go-fleet-core-and-e2e-provisioner-rewrite.md) |
| [0002](0002-no-orchestration-framework.md) | No orchestration framework (Hermes/OpenClaw rejected) | Superseded by [0017](0017-single-session-planning-pipeline.md) |
| [0003](0003-persistent-worker-pod-per-repo.md) | Persistent worker pod per target repo, not one Job per task | Accepted (crash-requeue gap closed by [0016](0016-task-crash-recovery-and-retry.md)) |
| [0004](0004-subscription-oauth-not-metered-api.md) | Claude Code subscription OAuth, not a metered API key | Accepted |
| [0005](0005-explicit-approval-gate.md) | Explicit human approval only gates write/edit unlock | Accepted |
| [0006](0006-git-identity-from-bot-account.md) | Git commit identity derived live from the authenticated bot account | Accepted |
| [0007](0007-two-source-argocd-application.md) | Two-source ArgoCD Application for independent deploy-tag bumps | Accepted |
| [0008](0008-unbounded-guardrail-defaults.md) | Guardrail defaults are unbounded, capped only opt-in | Accepted |
| [0009](0009-rtk-ponytail-baked-into-worker-image.md) | `rtk` + `ponytail` baked into the worker image | Accepted |
| [0010](0010-shared-rwx-pvc-across-apps.md) | Shared `ReadWriteMany` PVC across bot + both workers | Accepted |
| [0011](0011-critic-opt-out-and-context-handoff.md) | Critic session is opt-out (human-only), plus proposer→critic context handoff | Superseded by [0017](0017-single-session-planning-pipeline.md) |
| [0012](0012-e2e-provisioner-standalone-app.md) | On-demand e2e test environments via a standalone e2e-provisioner | Accepted |
| [0013](0013-go-fleet-core-and-e2e-provisioner-rewrite.md) | Go `fleet-core` replaces Redis coordination; `e2e-provisioner`/`bot` rewritten in Go | Accepted (two lines on gRPC/connect-es superseded by [0015](0015-connectrpc-dashboard-api.md)) |
| [0014](0014-fleet-core-dashboard-backend.md) | fleet-core as the web dashboard's backend | Accepted |
| [0015](0015-connectrpc-dashboard-api.md) | ConnectRPC replaces the dashboard's REST+SSE API | Accepted |
| [0016](0016-task-crash-recovery-and-retry.md) | Task crash recovery, heartbeat reclaim, and transient-error retry | Accepted |
| [0017](0017-single-session-planning-pipeline.md) | Single-session planner replaces proposer/critic | Accepted (Discord-as-interactive-user section superseded by [0018](0018-ask-user-question-via-dashboard.md)) |
| [0018](0018-ask-user-question-via-dashboard.md) | AskUserQuestion is real, answered via the dashboard, not Discord | Accepted |
