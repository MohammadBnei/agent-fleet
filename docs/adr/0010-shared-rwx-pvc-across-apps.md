# ADR-0010: Shared `ReadWriteMany` PVC across bot + both workers

**Status:** Accepted
**Date:** 2026-07-30

## Context

All three apps (`agent-fleet-bot`, `dream-analyst-worker`,
`vos-monolith-worker`) run in the same `agent-fleet` namespace. Each worker
already has its own `ReadWriteOnce` `/workspace` PVC for its git checkout
and per-task worktrees (never shared — see `DECISIONS.md` §2's "no shared
writable repo PVC across tasks"). Separately, there's a need for space
that genuinely *should* be shared across all three pods — common
skills/helper scripts, a knowledge-journal mirror, shared MCP configs —
without giving any pod write access to another's git working tree.

## Decision

One `ReadWriteMany` (Longhorn) PVC, `agent-fleet-shared-pvc`, declared as
an `extraManifests` block owned by the bot's Application
(`k8s/agent-fleet-bot.yaml`) since PVCs are only referenceable by pods in
the same namespace and the bot has the lightest lifecycle of the three.
Both worker values files mount it by claim name via `extraVolumes` at
`/mnt/fleet-shared` — distinct from and in addition to their own
`/workspace` PVC.

## Consequences

- Deleting/recreating the bot's Application also affects the shared PVC's
  lifecycle — the bot is not purely stateless despite having "no k8s API
  access needed" for its own Discord-ingress role.
- `/mnt/fleet-shared` is explicitly **not** a place for git working trees
  or anything that needs per-task isolation — that boundary must hold as
  usage of this mount grows, or it recreates the exact shared-writable-PVC
  failure mode `DECISIONS.md` forbids for task workspaces.
- Adding a fourth app to the fleet that needs this shared space just adds
  the same `extraVolumes`/`extraVolumeMounts` block — no new PVC needed.
