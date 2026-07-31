# Architecture Decision Records

One file per real decision — a competing alternative existed, a choice was
reversed, or a live incident forced a change. Format: Status / Date /
Context / Decision / Consequences. Status is one of `Proposed`, `Accepted`,
`Rejected`, `Superseded`.

Decisions with no real alternative ever on the table live in
[`../DECISIONS.md`](../DECISIONS.md) §1 instead, not here.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-redis-durable-list-over-pubsub.md) | Durable Redis list over pub/sub for the planning transcript | Accepted |
| [0002](0002-no-orchestration-framework.md) | No orchestration framework (Hermes/OpenClaw rejected) | Accepted |
| [0003](0003-persistent-worker-pod-per-repo.md) | Persistent worker pod per target repo, not one Job per task | Accepted |
| [0004](0004-subscription-oauth-not-metered-api.md) | Claude Code subscription OAuth, not a metered API key | Accepted |
| [0005](0005-explicit-approval-gate.md) | Explicit human approval only gates write/edit unlock | Accepted |
| [0006](0006-git-identity-from-bot-account.md) | Git commit identity derived live from the authenticated bot account | Accepted |
| [0007](0007-two-source-argocd-application.md) | Two-source ArgoCD Application for independent deploy-tag bumps | Accepted |
| [0008](0008-unbounded-guardrail-defaults.md) | Guardrail defaults are unbounded, capped only opt-in | Accepted |
| [0009](0009-rtk-ponytail-baked-into-worker-image.md) | `rtk` + `ponytail` baked into the worker image | Accepted |
| [0010](0010-shared-rwx-pvc-across-apps.md) | Shared `ReadWriteMany` PVC across bot + both workers | Accepted |
