# ADR-0028: dashboard-editable, DB-backed repo config

**Status:** Accepted
**Date:** 2026-08-06

## Context

The fleet's target-repo list (name, git URL, base branch) lived as a
hardcoded Go map, `tasks.KnownRepos` (`core/internal/tasks/store.go`),
duplicated by hand in four more places: Discord's `/task` command choices
(built once at process-init time), the dashboard's `NewTaskDialog` `<select>`
(a plain TS array, already stale — missing `agent-fleet`), and
`dashboard.Server.CreateTask`'s validation. Onboarding or editing a repo
meant editing Go source and redeploying `core` — documented as the official
process in `.claude/skills/fleet-ops/SKILL.md`'s "Onboard a new target
repo" section, deliberately code-review-gated per that skill's own note.

Mohammad asked for this to become dynamic: repo config set from the
dashboard, persisted in Postgres, with no redeploy needed to add, edit, or
remove a target repo. No prior ADR considered or rejected a DB-backed
version of this config — `docs/adr/0019`'s "onboarding = new Deployment/PVC"
concern is a different, already-solved problem (per-repo `Deployment`s were
replaced by the shared-PVC/provisioner redesign) — so this is new ground,
not a reversal of an earlier decision.

## Decision

### New `repos` table, `core/internal/repos` sibling store

Added a `repos` table (`db/schema.sql`, mirrored into `core/internal/db/
embedded_schema.sql`): `name TEXT PRIMARY KEY, url TEXT NOT NULL,
base_branch TEXT NOT NULL DEFAULT ''`, seeded with the three prior static
entries (`dream-analyst`, `vos-monolith`, `agent-fleet`) via an idempotent
`INSERT ... ON CONFLICT (name) DO NOTHING`. No FK from `tasks.repo` — a
removed repo shouldn't retroactively break historical task rows.

New `core/internal/repos` package, a `*pgxpool.Pool`-wrapping `Store`
(mirrors `journal.Store`'s shape, a sibling constructed alongside
`tasks.Store` rather than folded into it) with `List`/`Get`/`Create`/
`Update`/`Delete`, plus `SetOnChange(func())` — same optional-callback
pattern as `tasks.Store.SetNudge` — invoked after every mutation.

`tasks.KnownRepos`/`RepoConfig` deleted entirely. Every caller (`dispatch.
Loop.tick`, `dashboard.Server.CreateTask`) now calls `repos.Store.Get`
instead of a map lookup — both were already inside a request/tick handler,
so a DB round-trip there is not new latency-sensitive surface.

### New `DashboardService` RPCs: `ListRepos`/`CreateRepo`/`UpdateRepo`/`DeleteRepo`

Follows the existing proto conventions (verb+noun request/response pairs,
no pagination — the repos table is a handful of rows, unlike `tasks`/
`knowledge_journal`). `CreateRepo`/`UpdateRepo` return the mutated `Repo`
(matches `CreateTask`'s own precedent of returning the created resource);
`DeleteRepo` returns a `status` string (matches `DeleteTask`).

### Discord's `/task` dropdown refreshes live via `RefreshCommands`

`discord/commands.go`'s `commandDefs`/`repoChoices` became functions taking
`[]string` instead of a package-level var closed over the static map —
`discordgo.ApplicationCommandCreate` upserts by name, so re-registering is
safe to call repeatedly. `discord.Client` gained `RefreshCommands(ctx)`,
wired as `repos.Store`'s `OnChange` callback in `core/cmd/core/run.go`
(only when Discord is enabled) — a dashboard repo mutation propagates to
the live `/task` command choices without a bot restart, closing the same
"no redeploy" gap for Discord that the dashboard RPCs close for the web UI.

### Dashboard UI: `ManageReposModal`, reusing the existing `Modal` shell

No prior settings/admin page existed in the dashboard (confirmed by
exploration — `NewTaskDialog`/`BypassConfirmModal`/`ErrorModal` were the
only `Modal`-based components). Added `ManageReposModal.tsx`: a list of
existing repos with inline-editable url/base-branch fields + per-row
Save/Delete, plus an add-repo form, opened from a new header button next to
`NewTaskDialog`. `NewTaskDialog` itself now fetches `listRepos()` on open
instead of reading a hardcoded array.

### `ConfirmModal`: replaces `window.confirm`, typed-confirmation is opt-in per call site

While wiring `ManageReposModal`'s delete flow, discovered mid-implementation
that native `window.confirm` was still used in three places
(`App.tsx`'s task delete, `Worktrees.tsx`'s worktree delete, and the new
repo delete) — replaced all three with a new shared `ConfirmModal.tsx`
component (native dialogs can't be themed/tested and block the render
thread). `ConfirmModal` defaults to a plain Cancel/Confirm pair; passing a
`confirmWord` prop upgrades it to a typed-confirmation gate (mirrors
`BypassConfirmModal`'s require-typing pattern from ADR-0027) — this is
opt-in per call site, driven by whether `confirmWord` is passed, not a
blanket rule. Only the new repo-delete flow passes `confirmWord="delete"`
(a rarer, more structurally consequential action); task/worktree deletes
stay plain-click, matching the severity of their pre-existing
`window.confirm` UX.

One nesting bug found and fixed along the way: an early version rendered
each repo row's `ConfirmModal` nested inside `ManageReposModal`'s own open
`<dialog>`. Empirically, in this browser, closing the inner `<dialog>` via
its Cancel button also closed the outer one — confirmed via a minimal
`document.createElement('dialog')` repro that closing a nested dialog does
*not* close its ancestor when done outside React, so the bug was specific
to this app's DOM structure (both dialogs sharing one subtree), not a
general native-dialog quirk. Fixed by lifting the pending-delete state to
`ManageReposModal` itself and rendering `ConfirmModal` as a sibling of
`Modal`, never a descendant.

## Alternatives considered

- **Keep `KnownRepos` as a Go map, add a `/fleet-ops`-documented redeploy
  step for changes** — rejected: this is exactly the status quo Mohammad
  asked to change; a redeploy per repo edit is the friction being removed.
- **Fold repo config into `tasks.Store`** instead of a sibling `repos`
  package — rejected in favor of matching the existing `journal.Store`/
  `transcript.Store` precedent of one store package per table/concern.
- **A `repos.name` foreign key on `tasks.repo`** — rejected: would break
  historical task rows the moment a repo is deleted, for no query-time
  benefit (nothing joins `tasks` against `repos` today).

## Consequences

- `provisioner/internal/k8s/names.go`'s `StartCmdFor` (the e2e-preview
  start-command switch) is a *separate* hardcoded per-repo lookup, for a
  different purpose, deliberately left untouched — the provisioner holds no
  DB credentials (`docs/adr/0020` point 1) and architecturally cannot read
  the `repos` table directly. If this ever needs to become dynamic too, the
  start command would have to be threaded through as an RPC parameter from
  `core`, the same way `CreateWorkerPod`'s `repoUrl`/`baseBranch` already
  are.
- `core/internal/db/embedded_schema.sql` was already drifted from `db/
  schema.sql` before this change (missing `stop_requested_at` and the
  `permission_mode` transcript-type addition) despite a code comment
  claiming CI diffs the two — it doesn't, confirmed by reading the full
  `go.yml` workflow. This change kept the new `repos` DDL identical in both
  files but did not fix the pre-existing unrelated drift; worth a follow-up
  to either wire the claimed CI check for real or delete the stale comment.
- `.claude/skills/fleet-ops/SKILL.md`'s "Onboard a new target repo" section
  needed updating from the Go-source-edit-and-redeploy flow to the new
  dashboard-based one.
