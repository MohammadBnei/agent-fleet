# provisioner

Plain manifests (no Helm chart — RBAC/ServiceAccount binding isn't
something `common-app-chart` supports, and this needs its own tightly
scoped Role), applied as a standalone ArgoCD Application. Moved into this
repo from infra-bootstrap's `gitops/platform/e2e-provisioner/` as part of
`docs/adr/0019`/`0020`/`0021` — since this project's own deploy surface had
grown complex enough to warrant owning it directly rather than splitting
manifests across two repos. See `infra-bootstrap`'s
`gitops/bootstrap/provisioner-application.yaml` for the pointer Application
(its `source.repoURL`/`path` now point here instead of infra-bootstrap's
own `gitops/`), same pattern `gitops/platform/actions-runner/` uses for
things that don't fit the registry/`common-app-chart` mold.

Deploys into the existing `agent-fleet` namespace (already created by
infra-bootstrap's registry Applications) — no `namespace.yaml` needed here.

See `docs/adr/0012` for why this stays a standalone app at all, and
`docs/adr/0019`/`0020`/`0021` for the redesign this directory now reflects:
the provisioner isn't just the e2e-preview-pod spawner anymore — it owns
**all** cluster-pod creation (worker pods too) and the shared workspace
PVC's entire git lifecycle (clone/fetch/worktree add+remove), and holds
zero Postgres credentials (that moved to `core`, the fleet's sole DB
holder and the only thing that ever calls this service — hub-and-spoke).

## Contents

- `serviceaccount.yaml` — the identity the provisioner pod authenticates as.
- `role.yaml` — a namespaced `Role`+`RoleBinding` (never a `ClusterRole`),
  granting `create`/`get`/`list`/`watch`/`delete` on `Pod`/`Service` and
  `traefik.io` `IngressRoute`/`Middleware` in the `agent-fleet` namespace
  only. This is the **only** RBAC surface in the whole fleet that can
  create/delete cluster resources — `core` needs none at all (docs/adr/0020
  point 1), and worker pods themselves never get any of it either.
- `deployment.yaml` — a single provisioner pod, image built by this repo's
  own CI (`WORKER_IMAGE`/`SIDECAR_IMAGE` are the two containers it spawns
  as a worker pod, also built here and pinned/bumped the same way).
  `E2E_RUNNER_IMAGE` stays a floating `:latest` for v1 — it isn't built by
  this repo's CI, same accepted trade-off `actions-runner` documents for
  `myoung34/github-runner:latest`.
- `pvc.yaml` — the one shared RWX workspace PVC (docs/adr/0019) holding
  repo clones, per-task git worktrees, Claude Code sessions, and
  skills/`CLAUDE.md`. Replaces the old per-repo worker PVCs entirely.
- `service.yaml` — ClusterIP; `core`'s gRPC client reaches it at
  `provisioner.agent-fleet.svc.cluster.local:9090`.
- `infisicalsecret.yaml` — sources `GH_TOKEN` from the `agent-fleet-nygh`
  Infisical project (no DB credentials — those live in `core`'s own
  `k8s/core.yaml` now).
- `networkpolicy.yaml` — the first `NetworkPolicy` anywhere in this
  cluster. Scopes e2e-runner pods' inbound traffic only; worker pods aren't
  targeted by any policy and stay default-allow.

## One-time human setup (not something this repo/ArgoCD can do)

1. **Already done** — a `*.e2e.bnei.dev` wildcard A record points at the
   cluster's Traefik ingress (`infra-bootstrap` ADR-0033 moved `bnei.dev` to
   Cloudflare). Each task gets its own subdomain,
   `https://<shortId>.e2e.bnei.dev/`, serving the app at the **root** path with
   nothing stripped — code-server stays at `/code` on the same hostname
   (docs/adr/0038). Nothing to do per task; adding a preview needs no DNS change.

   The certificate is a single `*.e2e.bnei.dev` wildcard issued by Traefik's
   `le-dns` resolver (ACME DNS-01 via Cloudflare), also defined in
   `infra-bootstrap`. If previews ever fail TLS, check that resolver first —
   the failure appears in Traefik's ACME logs, not anywhere in this repo.
2. Apply the pointer Application in infra-bootstrap once (or let its
   `gitops/bootstrap/`'s self-sync pick it up — no manual `kubectl apply`
   needed beyond the one-time bootstrap already documented there).
