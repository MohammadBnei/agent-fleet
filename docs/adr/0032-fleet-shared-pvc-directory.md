# ADR-0032: PVC-resident, provisioner-synced fleet-shared skills/context

**Status:** Accepted
**Date:** 2026-08-10

## Context

Every worker pod's Claude Code capabilities — skills and shared context —
were frozen at Docker-image-build time: `worker/src/session.ts` hardcoded
`plugins: [{ type: "local", path: "/app/worker/skills/agent-fleet-planning" }]`,
baked in by `worker/Dockerfile`'s `COPY worker ./worker`. Adding or changing
a skill (`doubt-driven-development`, `architecture-interview`, both
required by ADR-0017) required a full image rebuild and redeploy.
ADR-0019 point 6 already proposed fixing this via a provisioner-seeded,
PVC-resident shared directory — accepted, but never implemented.

Reached via an architecture-interview with Mohammad (this decision's
alternatives section reflects that discussion directly).

## Decision

Replace the worker's image-baked skills plugin with a PVC-resident,
git-synced directory (`fleet-shared/`, this repo) that the **provisioner**
syncs before each worker pod starts (`git.Manager.SyncFleetShared`), mirrored
into `$CLAUDE_CONFIG_DIR` (the shared PVC path already used for per-task
session resume, ADR-0029 point 5). The worker (`worker/src/session.ts`) sets
`settingSources: ["user"]` so Claude Code's own native discovery finds
`CLAUDE.md`/`settings.json`/`skills/` there — no `plugins:` entry, no
per-item registration. Adding a skill becomes a PR merge to `fleet-shared/`;
zero `session.ts` change, zero worker image rebuild.

**Quality attribute priority:** flexibility/extensibility over explicit
control. What loads in a given session is now whatever's currently merged
to `fleet-shared/`, discovered natively — not a fixed list enumerated in
code.

**Constraints respected:**
- `core` remains the sole Postgres-credential holder — this doesn't touch
  `knowledge_journal` or any DB table.
- The provisioner remains the only RBAC'd, PVC-git-lifecycle component —
  it's the one syncing `fleet-shared/`, the same pattern as target-repo
  worktrees (ADR-0019 point 2).
- No new inter-pod protocol: the sync happens once per dispatch, before the
  worker pod exists — not a live channel between running pods, so ADR-0020
  point 6 ("gRPC is the only inter-process/inter-pod protocol") holds.

## Alternatives considered

- **Status quo (image-baked plugin).** Rejected — it's the exact
  rebuild-per-change cost this decision removes.
- **Explicit per-plugin path entries in `session.ts`.** Solves "new skill
  nested under an existing plugin" but still forces a code change +
  redeploy for a genuinely new plugin bundle. Rejected — doesn't meet the
  stated flexibility goal.
- **A live, multi-writer PVC file as a "real-time shared journal/memory"
  across tasks.** Raised during the interview as a way for workers to
  "come alive in real time." Rejected: it would duplicate the existing
  Postgres `knowledge_journal` table and add a second inter-pod
  coordination path outside `core`'s gRPC gate, contradicting the already
  -locked ADR-0020 point 6 decision. `knowledge_journal` already exists for
  this purpose (core-owned, gRPC-gated); it just doesn't feed back into a
  session's prompt yet — that's separate, deferred work, not solved here.
- **Marketplace-installing `ponytail`/`caveman` as live plugins at every
  pod startup.** Considered, then explicitly ruled out by Mohammad: running
  `claude plugin marketplace add`/`install` on every dispatch means a
  network git-clone on every pod start, for no benefit once already
  installed once on the shared PVC. Deferred — needs an install-once/
  refresh-on-demand mechanism, not a per-dispatch clone.

## Consequences

- Redeploy-free growth of shared skills/context, at the cost of looser
  auditability — a session's exact loaded context depends on what's
  currently merged to `fleet-shared/` at sync time, not a fixed list in
  code.
- `SyncFleetShared` failures are non-fatal to a dispatch (log + warn,
  continue) — a worker pod can start with stale or momentarily-missing
  shared content rather than failing the whole task. This differs
  deliberately from `EnsureRepoCloned`/`CreateWorktree`'s fail-the-dispatch
  behavior, since shared context is a convenience layer, not the target
  repo itself.
- `worker/Dockerfile`'s build-time `claude plugin marketplace add
  DietrichGebert/ponytail && claude plugin install ponytail@ponytail` step
  (and its `/root/.claude/settings.json` copy) is left in place but remains
  inert — it always was, since no `settingSources` was configured at all
  before this change (full SDK isolation mode). This change doesn't fix
  that; it's explicitly out of scope (see below).

## Out of scope / deferred

- Per-repo toolchain provisioning (buf/go/bun, etc., tailored per target
  repo) — a separate decision.
- Feeding `knowledge_journal` back into a worker's prompt — existing
  mechanism (core-owned, gRPC-gated), just needs a read path built later.
- General file exchange between a human and an agent (arbitrary reference
  files/assets, not skills/context) is a separate, already-accepted
  mechanism — [ADR-0031](0031-garage-s3-shared-files.md), Garage
  S3-backed, presigned URLs minted only by `core`, no PVC involvement at
  all. No overlap with this decision: that ADR governs ad hoc file
  exchange; this one governs what a session's own Claude Code config
  discovers at startup.
- Making `ponytail`/`caveman` live marketplace plugins — needs its own
  install-once-and-persist design, not a per-dispatch marketplace clone.
- Whether `settingSources` should ever include `"project"` (loading the
  *target repo's own* `.claude/settings.json`/config into a worker
  session) — a separate decision, not made here.
- A `plugins/` subdirectory in `fleet-shared/` — nothing today needs a
  bundle beyond a plain skill; add when something requires custom
  commands/hooks.

## Reversibility

One-way door — once worker pods depend on a fixed `CLAUDE_CONFIG_DIR` mount
with native-discovery semantics, changing the loading mechanism again means
touching how every worker pod resolves its skills/context. The *contents*
placed inside `fleet-shared/`, by design, stay cheap and additive.
