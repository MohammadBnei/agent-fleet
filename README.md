# agent-fleet

A self-hosted fleet of Claude Code workers: each owns one feature end-to-end
(plan → code → tests → docs), triggered from Discord, running on
`ukubi-cluster`.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that's where the cluster itself (Kubernetes, ArgoCD,
ingress, secrets backend) is provisioned and documented. This repo holds the
fleet's own design and, once implementation starts, its source (Discord bot,
worker Job template, k8s manifests).

## Status

Active work is scoped by **[`mvp-spec.md`](./mvp-spec.md)** — the MVP golden
path: one Discord message → one isolated Claude Code worker → one PR. No
code exists yet; this is the spec phase.

**[`design-v0.md`](./design-v0.md)** is the original, broader v2 design doc
(first draft) — full fleet vision including staging/production gating, a
production bug-handling loop, and an orchestration-layer decision (Hermes vs
OpenClaw vs plain Claude Code) that's intentionally deferred. It's parked for
context, not the active spec. `mvp-spec.md` supersedes it for anything being
built right now.

## Relationship to `infra-bootstrap`

- The cluster (`ukubi-cluster`), GitOps (`gitops/`), and secrets backend
  (External Secrets + SOPS/age) are all owned by `infra-bootstrap` — this
  repo consumes them, it doesn't redefine them.
- Workers run as Kubernetes Jobs on `ukubi-cluster`; the Discord bot deploys
  as a normal gitops app once it exists, following `infra-bootstrap`'s
  `/add-app` pattern (reusing `gitops/platform/common-app-chart`).
- Per `infra-bootstrap`'s own `CLAUDE.md`, this fleet does **not** manage
  `infra-bootstrap`'s own cluster ops (kubespray/ansible/pigsty) — that's
  explicitly blocked until revisited (see `design-v0.md` §11.4).
