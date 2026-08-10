# ADR-0033: Journal search/write tools, and a persistent worker-plugin mechanism

**Status:** Accepted
**Date:** 2026-08-10

## Context

`docs/adr/0032` shipped the PVC-resident `fleet-shared/` directory but
explicitly deferred two gaps it identified along the way:

1. `knowledge_journal` only ever received machine-written lifecycle events
   (`task.claimed`/`stopped`/`transient_error`/`failed`) — no agent-authored
   entries, and no retrieval beyond a plain cursor scan
   (`journal.Store.List`). ADR-0032's own "out of scope" list: *"Feeding
   knowledge_journal back into a worker's prompt — existing mechanism,
   just needs a read path built later."*
2. `worker/Dockerfile` bakes `ponytail` into the image at build time via
   `claude plugin marketplace add`/`install`, but that writes into the
   image's default `$HOME` (`/root/.claude`). At runtime,
   `worker/src/session.ts` sets `CLAUDE_CONFIG_DIR` to a PVC path
   (`/workspace/.claude-home`), which fully redirects Claude Code's plugin
   discovery away from where the image put it — ADR-0032 flags this as
   "remains inert." Fixing it was explicitly deferred: *"needs an
   install-once/refresh-on-demand mechanism, not a per-dispatch
   marketplace clone"* — ADR-0032 also explicitly rejected running
   `claude plugin marketplace add`/`install` on every dispatch (a network
   git-clone on every pod start, for no benefit once already installed
   once).

## Decision

### Journal search + write

- New `SearchJournal` RPC on `CoreService` (`core/internal/journal/store.go`
  `Store.Search`), using Postgres full-text search
  (`to_tsvector`/`plainto_tsquery`/`ts_rank`, backed by a new GIN index,
  `db/migrations/000002_journal_fts.up.sql`) — no embedding model, no
  vector store. `core` already owns Postgres exclusively (ADR-0020 point
  1); this is a query, not a new credential holder.
- `JournalEntry` moved from `dashboard.proto` into `core.proto` (same
  reuse pattern as `Task`/`GetTaskRequest`/`GetTaskResponse`, noted
  in-file) so both `GetJournal` (dashboard) and the new `SearchJournal`
  (core) share one message — `buf breaking`'s `PACKAGE` scope (not
  `FILE`) treats this as wire-compatible, per its own comment anticipating
  exactly this pattern.
- Two new sidecar MCP tools, same proxy-through-`core` shape as every
  other tool in `sidecar/internal/mcpserver/server.go`:
  `journal_write(repo, note)` (event_type `"agent_note"`, distinguishing
  agent-authored entries from lifecycle events) and
  `journal_search(repo, query, limit)`.
- `fleet-shared/CLAUDE.md` tells the agent when to use them: search before
  starting non-trivial work on a repo, write when something's worth a
  future session knowing.

### Persistent plugins (ponytail + caveman)

- `worker/entrypoint.sh`: a guarded one-time copy —
  `[ -d "$CLAUDE_CONFIG_DIR/plugins" ] || cp -r /root/.claude/plugins
  "$CLAUDE_CONFIG_DIR/plugins"` — run before the worker process starts. No
  network call, no `claude plugin add`/`install` at runtime; the Dockerfile
  already installs both plugins correctly at build time, this just
  relocates those files onto the PVC once. Every later dispatch on the
  same PVC sees `plugins/` already present and skips straight to `exec`.
  Purely worker-side — no provisioner change, since `CLAUDE_CONFIG_DIR` is
  already a mounted PVC path in the worker container.
- `worker/Dockerfile` gains a second build-time install step for
  `JuliusBrussee/caveman`, alongside the existing `ponytail` one.
- `fleet-shared/settings.json` gains `enabledPlugins` +
  `extraKnownMarketplaces` entries for both plugins. Necessary because
  `SyncFleetShared` fully overwrites `$CLAUDE_CONFIG_DIR/settings.json`
  from this file every dispatch — the entrypoint script only gets the
  plugin *files* onto the PVC, `enabledPlugins` in `settings.json` is what
  Claude Code actually reads to turn a plugin on.

**Quality attribute priority:** for the plugin fix, reuse over
reinvention — the existing build-time install already works, so the fix
is "make its output reachable at runtime," not a second install
mechanism.

## Alternatives considered

- **Vector embeddings for journal search.** Rejected (explicit user
  choice) — a real infra addition (embedding-generation calls, pgvector
  or a vector store) for a small, append-only log at this fleet's current
  scale. Postgres full-text search is BM25-family ranking already built
  into the one datastore `core` exclusively owns.
- **Provisioner-driven plugin install** (extend `SyncFleetShared` to also
  materialize plugins). Rejected — the provisioner is a Go binary with no
  `claude` CLI; hand-constructing `installed_plugins.json`/
  `known_marketplaces.json` from Go would duplicate Claude Code's own
  internal, undocumented format and break silently on a Claude Code
  update. The worker container already has the CLI and the correctly
  -installed files; copying is simpler and doesn't touch that format at
  all.
- **Re-running `claude plugin marketplace add`/`install` every dispatch.**
  Already explicitly rejected in ADR-0032 (network clone on every pod
  start, no benefit once installed once) — this ADR implements the
  install-once alternative ADR-0032 named instead.

## Consequences

- A concurrent first-boot race (two pods copying `plugins/` onto an empty
  PVC simultaneously) is possible but harmless — both copies come from the
  same read-only image layer, so a double-copy is a no-op collision, not
  corruption. No lock added (`ponytail:` comment in `entrypoint.sh` names
  this as the accepted shortcut).
- Adding a third plugin later means both a new `worker/Dockerfile` install
  step *and* a new `fleet-shared/settings.json` `enabledPlugins` entry —
  two places, not one. Not worth collapsing into a generic mechanism for
  two plugins.

## Out of scope / deferred

- A generic N-plugin registry (config-driven list instead of one
  Dockerfile line + one settings.json entry per plugin) — revisit if a
  third plugin is ever added.
- Refreshing an already-copied plugin to a newer version without a full
  PVC wipe — today's guard is existence-only, not version-aware.
