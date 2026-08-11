# DECISIONS

Settled-decisions log for `agent-fleet`. This is the WHY: rationale for
choices that were never really in question, plus a quick-reference list of
things not to propose. It is **not** a spec doc — topology and current
features live in [`ARCHITECTURE.md`](ARCHITECTURE.md). Full alternative-
weighing for anything that had real competing options lives in
[`adr/`](adr/README.md), one file per decision, independently trackable by
status.

Any doc, code, comment, or memory that contradicts this file or an
`Accepted` ADR is overridden by them until the file/ADR itself is updated.

## Reading order (for AI agents)

1. This file (`DECISIONS.md`) — hard prerequisite.
2. [`ARCHITECTURE.md`](ARCHITECTURE.md) for topology/current features.
3. [`adr/`](adr/README.md) for the reasoning behind any specific decision.
4. Then the source itself (`core/internal/`, `provisioner/internal/`,
   `sidecar/internal/`, `worker/src/`).

---

## 1. Locked decisions (no dedicated ADR — never really in question)

- **Bun is the sole JS runtime for `worker/`** — the only remaining TS/Bun
  component (superseded 2026-08-03 by [`adr/0013`](adr/0013-go-fleet-core-and-e2e-provisioner-rewrite.md)
  for `bot/`/`mcp-redis/`, which no longer exist; `core`/`provisioner`/
  `sidecar` are all Go, per [`adr/0019`](adr/0019-shared-pvc-and-unified-provisioner.md)–[`0021`](adr/0021-continuous-streaming-session.md)'s
  rename/redesign of `fleet-core`/`e2e-provisioner` plus the new `sidecar`
  component). No build step for `worker/`'s TypeScript sources — runs
  directly. No npm/pnpm/yarn workspace tooling; `worker/` keeps its own
  `package.json`/`bun.lock`, and each Go module keeps its own `go.mod`
  under the shared `go.work`.
- **Git auth goes through the `gh` CLI**, not a hand-rolled token-in-header
  client — `gh auth setup-git` wires `GH_TOKEN` into git's credential
  helper once, and `gh pr create`/`gh api` reuse the same token. Both the
  provisioner (its own clone/fetch on the shared PVC) and the worker (its
  own push/PR) configure this independently — separate pods, no shared
  `$HOME`.
- **`knowledge_journal` is append-only**, not a mutable shared doc — avoids
  write-conflict issues a shared mutable record would hit across
  concurrently dispatched worker pods. Written only by `core` now (`core`
  is the fleet's sole Postgres-credential holder,
  [`adr/0020`](adr/0020-hub-and-spoke-grpc-worker-sidecar.md) point 1) —
  every other component appends to it via a `CoreService` gRPC call, not a
  direct write.
- **One repo, one version/CHANGELOG.** `release.yml` runs from the repo
  root, not per-package — the fleet ships as one unit even though
  `worker/`, `core/`, `provisioner/`, `sidecar/` are deployed as separate
  images.
- **Workflow discipline:** all changes via feature branch + PR, no direct
  push to `main`; secrets only via Infisical, fetched at run time, never
  committed.
- **`core` is the sole holder of the Garage S3 credential** backing the
  fleet-wide shared file space — it only ever mints short-lived presigned
  PUT/GET URLs, never proxies file bytes itself. See
  [`adr/0031`](adr/0031-garage-s3-shared-files.md).
- **`thot` (design decided, not yet built) is the fleet's one named
  exception to hub-and-spoke** — a second, independently GitOps-deployed
  RBAC holder, reachable directly by worker sidecars/alerts/humans over its
  own protobuf/gRPC service, never proxied through `core`. Its RBAC may
  never include `rbac.authorization.k8s.io` verbs, blanket `secrets` read,
  or node-level verbs, and it must never target `core`/`provisioner`'s own
  pods or a pod holding an active git-worktree lock. `provisioner` never
  creates `thot`'s credentials — that would be a privilege-escalation
  primitive `provisioner` doesn't have today. `core` stays the sole
  Postgres-credential holder regardless — `thot`'s findings still persist
  through it. See [`adr/0035`](adr/0035-thot-cluster-agent.md).
- **A repo's e2e recipe lives in `repo_profiles` and is a human's to
  change.** The agent reads the resolved recipe back from
  `request_e2e_env`; a `start_cmd` override needs an explicit human yes,
  applies to that one task, and is never written back to the profile. See
  [`adr/0036`](adr/0036-e2e-recipe-visible-and-override-approved.md).

## 2. Forbidden patterns (quick check — full list + reasons in `adr/`)

- **A shared writable repo PVC across tasks.** One git worktree per task,
  always — see `adr/0003`.
- **Inferring a permission decision from silence, round completion, or
  free-text sentiment.** `canUseTool` prompts live and blocks for a real,
  structured `RespondToPermission` reply (or an explicit
  `SetPermissionMode` call, itself typed-confirmation gated for
  `bypassPermissions`) — never inferred from anything else. `/approve` no
  longer exists — see `adr/0005`, `adr/0027`, `adr/0029`.
- **Deleting a worktree or branch as a side effect of a task reaching a
  terminal status.** Only an explicit signal — the sweep's confirmed
  `[gone]`, or an explicit dashboard delete — removes git state; a hard-won
  lesson after uncommitted work was destroyed twice by the old design —
  see `adr/0023`.
- **An agent silently substituting its own e2e start command for the
  repo's profile.** The override is gated on a real human answer and never
  persists; declining, timing out, or a malformed answer all fall back to
  the profile — see `adr/0036`.
- **Hardcoding git commit identity.** Always derived live from the
  authenticated bot GitHub account — see `adr/0006`.
- **Committing Discord/GitHub/Anthropic tokens** to this repo or any
  target repo, in code, manifests, or CI config.
- **A bespoke per-app Helm chart.** Always reuse
  `infra-bootstrap/gitops/platform/common-app-chart` — see `adr/0026`.
- **A bare streaming-watch RPC without a durable resume cursor for the
  transcript.** Pull/cursor reads only — see `adr/0013`
  (successor to the same message-loss concern `adr/0001` raised about
  Redis pub/sub, now against a gRPC/Postgres backend instead of Redis).
- **A hand-rolled `CREATE TABLE`/partial-schema test fixture, or a second
  copy of the schema anywhere outside `db/migrations/`.** This exact
  pattern caused two separate live incidents (a missing `guidance` column,
  then a missing `suggested_permission_mode` column that shipped
  undetected because the copy that mattered was never updated). Every
  integration test uses `core/internal/dbtest.NewPool(t)`, which applies
  the real `db/migrations/` via golang-migrate — see `adr/0030`.
- **An orchestration framework (Hermes, OpenClaw, or similar).** A single
  Agent SDK planning session, using real Claude Code skills
  (doubt-driven-development, architecture-interview) for structured
  review/elicitation instead of a second independent session or an
  external orchestrator — see `adr/0002` (superseded) and `adr/0017`.
- **Fleet-managed `infra-bootstrap` cluster ops.** That repo's own
  `CLAUDE.md` states a human runs kubespray/ansible/pigsty personally;
  this fleet does not touch that, full stop, until that decision is
  explicitly revisited in `infra-bootstrap` itself.

Don't propose any of these without an explicit greenlight from Mohammad,
even as a "better alternative" — each one was tried, considered, or hit a
live incident that's recorded in `adr/`.
