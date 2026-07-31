# ADR-0007: Two-source ArgoCD Application for independent deploy-tag bumps

**Status:** Accepted
**Date:** 2026-07-30

## Context

`agent-fleet`'s deploy config (`k8s/*.yaml`, Helm values) needs to live
somewhere, and `infra-bootstrap`'s Pattern C GitOps convention (ADR-0004 in
that repo) expects one registry entry per app plus reuse of
`gitops/platform/common-app-chart`. The naive option is to also put the
values files inside `infra-bootstrap`'s own `gitops/` tree — but this
repo's own CI (`docker.yml`) needs to bump the pinned image tag on every
release, and doing that would require a cross-repo commit from
`agent-fleet`'s CI into `infra-bootstrap` on every single release.

## Decision

Each of the three apps (`agent-fleet-bot`, `dream-analyst-worker`,
`vos-monolith-worker`) is registered in `infra-bootstrap`'s
`gitops/apps/registry.yaml` as a **two-source** Application: the Helm
chart comes from `infra-bootstrap` (`gitops/platform/common-app-chart`),
but the values file comes from *this* repo's `k8s/`. `docker.yml`'s
`deploy` job commits the tag bump straight to `agent-fleet`'s own `main`.

## Consequences

- `docker.yml`'s deploy job never needs write access to `infra-bootstrap`
  — only to its own repo.
- ArgoCD syncs from two repos for these three apps; both must be reachable
  and correctly configured as Application sources, or the sync fails.
- Registering a new target repo/worker still requires one edit to
  `infra-bootstrap`'s `gitops/apps/registry.yaml` (via that repo's
  `/add-app` skill) — this ADR only removes the *tag-bump* commit from
  crossing repos, not the one-time registration.
