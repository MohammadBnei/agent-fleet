# ADR-0030: golang-migrate replaces the hand-copied `db/schema.sql` + test-fixture schemas

**Status:** Accepted
**Date:** 2026-08-09

## Context

`core/internal/db/embedded_schema.sql` was a hand-maintained, `go:embed`'d
copy of the canonical `db/schema.sql` — `go:embed` can't reach outside its
own package directory, so keeping the two byte-identical was a manual
discipline, not something enforced. Commit `1ed8dc9` ("use database as
source of truth for task configuration," authored by the fleet's own
`argocd-ukubi-bot` self-edit identity) added a `suggested_permission_mode`
column to `db/schema.sql` and, over several follow-up commits, updated
every *test* fixture that hand-rolled a subset of the schema — but never
updated `embedded_schema.sql`, the one copy `core` actually runs against
real Postgres via `ApplySchema`. Every real/kind-deployed database was
therefore missing the column, and any code path touching it
(`ListPromptSnippets`, `CreateTask`'s guidance/mode resolution) broke with
a live "column does not exist" error — undetectable by `go build`/`go
vet`/`go test ./...` (non-integration), since none of those touch a real
Postgres.

`core/internal/db/migrate.go` carried a comment claiming "CI diffs the two
to catch drift" — that check never actually existed in
`.github/workflows/go.yml`. The safety net the codebase believed it had was
fictional.

This was also not a one-off mistake. A prior incident already forced fixing
4 hand-rolled `CREATE TABLE tasks (...)` test fixtures for a missing
`guidance` column. Re-auditing at this incident found **7** files
independently hand-rolling their own partial schema for a testcontainers
Postgres (`core/cmd/core/activity_store_integration_test.go`,
`core/internal/repos/store_test.go`, `core/internal/tasks/store_test.go`,
`core/internal/transcript/postgres_test.go`,
`core/internal/dashboard/stop_integration_test.go`,
`core/internal/promptsnippets/store_test.go`,
`core/internal/coreserver/server_test.go`) — each independently able to
drift, with no drift check at all on the 6 that weren't `tasks`-shaped —
plus 3 more test files in the same packages (`transcript/relay_test.go`,
`dashboard/create_task_integration_test.go`,
`dashboard/warm_integration_test.go`) that shared one of those hand-rolled
pools and would have silently broken the moment it became the real schema
(bare `INSERT INTO tasks DEFAULT VALUES` against now-`NOT NULL`
`repo`/`description`; `INSERT INTO repos` colliding with real seed rows). A
CI diff-check scoped to just `embedded_schema.sql` would not have caught
any of this, and wouldn't catch the *next* copy either. The actual bug was
structural: N hand-maintained copies of one schema, with detection only as
good as whichever copies someone remembered to guard.

## Decision

Replace `db/schema.sql` + `embedded_schema.sql` + every hand-rolled test
fixture with **golang-migrate reading one directory of versioned migration
files (`db/migrations/000N_*.up.sql`/`.down.sql`) as the sole source of
truth**, mirroring a pattern already proven in a sibling fleet repo
(`vos-monolith/back`):

1. `db/migrations/000001_init.up.sql`/`.down.sql` capture the schema's
   end-state as of this cutover — not a replay of `db/schema.sql`'s
   historical `ALTER`s. Every future schema change is a new numbered
   migration file; an already-applied file is never edited again.
2. A new, minimal `migration/` image (`FROM migrate/migrate:latest` +
   `COPY db/migrations /migrations`, no custom Go binary, no new
   dependency inside `core` itself) is what actually applies migrations —
   at real-deploy time via `k8s/core.yaml`'s `hooks.migrate` PreSync job
   (previously `core`'s own `migrate` subcommand), and via `kind-local`'s
   equivalent one-off Pod. `core` no longer embeds or applies any schema;
   `core/internal/db/migrate.go`, `embedded_schema.sql`, and the `migrate`
   subcommand in `core/cmd/core/main.go` are deleted outright.
3. A new shared `core/internal/dbtest` package spins up a real
   testcontainers Postgres and applies the real `db/migrations/` via
   golang-migrate's Go library — every integration test that previously
   hand-rolled its own `CREATE TABLE` now calls `dbtest.NewPool(t)`
   instead. This collapses 7 independently-drifting copies (plus the 3
   dependent test files above) into 1 helper that can never drift from the
   real schema, because it *is* the real schema.

Quality-attribute call, reached via an architecture interview rather than
patching the symptom: correctness/single-source-of-truth now outranks this
codebase's usual zero-dependency-simplicity default for schema management
specifically — but bounded. Adopt exactly the sibling repo's
already-validated shape; no rollback orchestration, no schema-versioning
ceremony, no ORM, beyond what golang-migrate's own convention already
gives for free.

## Alternatives considered

- **Auto-generate `embedded_schema.sql` from `db/schema.sql` at build time
  (symlink or a small script)** — rejected: fixes only the
  core-vs-canonical drift axis, leaves every hand-rolled test fixture
  drifting independently, exactly as it already had before this incident.
- **Keep the current architecture, add the CI diff-check the code already
  falsely claimed to have, and extend it to the test fixtures** — rejected:
  doesn't scale, every new hand-rolled copy needs its own bespoke guard
  added after the fact, and it treats a structural problem as a checklist
  problem.
- **A heavier migration framework with rollback orchestration, schema
  versioning APIs, or an ORM** — rejected as over-building for a
  single-Postgres-instance, single-operator, homelab-scale tool; the goal
  was one source of truth, not migration tooling for its own sake.

## Consequences

- `core` is smaller and holds zero schema-related code or dependencies —
  `db.ApplySchema`, the `migrate` subcommand, and the embedded copy are
  gone; only the new `migration` image (and `core/internal/dbtest`, test-
  only via its own build tag) depend on golang-migrate at all.
- A new deploy artifact exists: `migration/Dockerfile`, built and pushed by
  `.github/workflows/docker.yml`'s new `build-push-migration` job, gated
  into the `deploy` job's `needs:` so a release's `hooks.migrate.image.tag`
  bump can never point at a tag that wasn't actually pushed.
- `db/schema.sql` no longer exists. A schema change is now "add a new
  `db/migrations/000N_*.up.sql` + `.down.sql`," never "edit an existing
  file" — this is a one-way door on convention, not on data: reversing it
  later means re-touching every environment that bootstraps a DB from it,
  but the risk is low since the pattern is already live elsewhere in this
  operator's infra.
- Every integration test that used to hand-roll a schema subset now
  exercises the *real* schema, including columns/constraints/seed data its
  original author never anticipated needing — this is a feature (it would
  have caught this exact incident), not a cost, though a few tests needed
  fixture-name adjustments to avoid colliding with `db/migrations/`'s
  seeded `repos`/`prompt_snippets` rows.

## Not superseded (still holds, noted for cross-reference)

- **ADR-0028** — dashboard-editable, DB-backed `repos` config is unchanged;
  its seed rows now live in `db/migrations/000001_init.up.sql` instead of
  `db/schema.sql`, same data, same seeding mechanism (`INSERT ... ON
  CONFLICT DO NOTHING` inside the migration, not a separate step).
