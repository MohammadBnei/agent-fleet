# ADR-0055 — The journal is agent knowledge, not pod telemetry

- **Status:** Accepted
- **Date:** 2026-08-18
- **Amends:** [ADR-0033](0033-journal-search-and-persistent-plugins.md)
  (`journal_search`'s signature) and
  [ADR-0024](0024-crash-fast-path-and-journal-read.md)'s journal-write
  half of `ReportPodEvents` (the pod event *stream* and `GetJournal` stand)

## Context

Issue #198 was filed by an editable-blog session writing that repo's weekly
rundown — a report on what happened across `agent-fleet`, `infra-bootstrap`
and `editable-blog` in the last seven days. `gh` gives it merged PRs and
commits. What it cannot give is what a session *skipped, deferred, or
knowingly left broken*; that material exists only in `knowledge_journal`, in
`agent_note` and `session.stopped` payloads, and it is the entire point of the
report's "what stalled" section.

The natural query is *every entry, all repos, created in the last seven days*.
`journal_search` could not express it:

- `repo` was `mcp.Required()`, so it was one call per repo — and a newly
  onboarded repo silently drops out of the report until someone edits the
  skill.
- `query` was `mcp.Required()`, so the caller had to guess search terms for
  entries it has not read yet. Relevance ranking is precisely the wrong
  ordering here: it surfaces what matches the guess, and a one-off gotcha
  nobody thought to phrase that way is invisible.
- There was no date filter at all, so whatever came back had to be filtered on
  `createdAt` client-side — after ranking had already decided what the caller
  got to see.

None of that was a storage limitation. `journal.Store.Search` and `List` both
treated `repo == ""` as "match every repo" and said so in their doc comments.
The restriction lived entirely in the tool definition and its handler's
`repo and query are required` guard.

So the skill did what a blocked caller does — it went around:

```bash
curl -s -X POST "http://$AGENT_FLEET_CORE_SERVICE_HOST:8080/agentfleet.v1.DashboardService/GetJournal" \
  -H 'Content-Type: application/json' -H 'X-Fleet-Dashboard: 1' \
  -d '{"repo":"","sinceId":0,"limit":100000}'
```

468 rows, 186 KB, filtered down to 36 useful entries in the client. It works
only because `X-Fleet-Dashboard` is a CSRF guard, not authorization — so an
agent gets the dashboard's whole RPC surface, mutating methods included, to
perform one read. That exposure is real and is tracked separately as #200;
this ADR removes the *reason* to reach for it.

### The 80% nobody read

Of those 468 rows, ~80% were `pod.POD_PHASE_*`, written by `applyPodEvent` on
every pod lifecycle transition the provisioner reported.

The issue proposed excluding them by default, or adding an `eventType` filter.
Tracing the write instead turned up something better: **nothing reads them.**
No dashboard view calls `GetJournal` at all — the RPC and its generated client
exist and the UI never invokes them. No session has ever searched for a pod
row. And the state they encode is not lost without them: the same handler
writes `sessions.pod_phase` two statements later, which is the live value the
reconcile loop, the concurrency cap and the topology all read, while the
event history is in Loki.

The cost of the write landed entirely on the one caller that did want to read
the journal: under any `LIMIT`, four rows of telemetry crowded out every row
of signal.

## Decision

1. **`journal_search(repo?, query?, since?, until?, limit?)`** — every argument
   optional.
   - `repo` omitted matches every repo (unchanged store semantics).
   - `query` omitted drops the full-text predicate and returns the window
     reverse-chronologically.
   - `query` present keeps relevance ranking, now bounded by the window.
   - `since`/`until` accept RFC3339 or `YYYY-MM-DD`, and bound the ranked path
     too.
   - Results carry `id` and `repo`; with `repo` optional, an entry the caller
     cannot attribute is not usable.
2. **`applyPodEvent` no longer writes to `knowledge_journal`.** `SetPodPhase`
   is untouched.
3. **The worker no longer writes `session.started`/`stopped`/`failed`
   either**, and the crash error moves to the transcript rather than being
   dropped with them — see "Lifecycle is not knowledge" below.
4. **The existing `pod.%` and `session.%` rows are deleted by a one-off manual
   `DELETE`, not a migration.** They are cleanup of what a past mistake wrote,
   not a schema change.

### Lifecycle is not knowledge

The first pass of this ADR removed `pod.*` and stopped there. Asked why
`session.*` was any different, the honest answer was that it is not — those
three event types are also each written in exactly one place and read by
nothing. But they were not equally worthless, and the difference is the
decision:

- **`session.started` carried `{sessionId}` and nothing else** — a fact
  `sessions.created_at` and `pod_phase` both already hold. It was 59 of the 98
  rows left after the `pod.*` purge, so the journal was *still* 60% lifecycle
  noise. Deleted.
- **`session.stopped`'s "summary" is `result.summary`**, which is `finalText`
  (`worker/src/session.ts:1102`) — the last assistant text block. Every
  assistant text block was already pushed to `transcript` verbatim as a
  `discussion` entry (`:408`). It is a copy of a row that already exists, and
  "whatever the agent happened to say last" is as often a question as a
  result. Deleted.
- **`session.failed`'s error string existed nowhere else durable.** It is
  thrown outside the SDK loop so `transcript` never saw it, `pod_message` gets
  only the generic `"worker job reached a terminal Failed phase"`, and Loki is
  retention-bound. So it **moves** rather than dying: the worker pushes
  `{sdk:"session_failed", error}` as a `system` transcript entry, reusing the
  shape `session.ts:402` already uses for an SDK assistant error.

Putting it on the transcript is the better home regardless of this ADR: the
dashboard renders the transcript and has never rendered the journal at all.
An irreplaceable record is worth relocating; it is not worth deleting to make
a rule tidier.

### Two details that are decisions, not implementation

**Newest-first, not the ascending order the issue asked for.** With a `LIMIT`,
ascending truncates a seven-day window to its *oldest* rows and drops exactly
what the caller came for. A caller wanting narrative order reverses a page it
already has; a caller given the wrong page cannot recover it.

**A bare `YYYY-MM-DD` `until` covers that whole day.** `until` is an exclusive
bound, which is right for an instant and wrong for a date:
`since=2026-08-11&until=2026-08-18` is the literal seven-day window an agent
writes, and read as a raw exclusive timestamp it drops every entry from the
18th — the day the report most wanted. Silent, because a shorter result set
looks exactly like a quieter week. A full RFC3339 `until` named an instant and
stays exclusive. This shipped as a bug in PR #199 and was caught in review;
the branch's own test had asserted the buggy window.

## Alternatives considered

- **Leave the `GetJournal` curl as the supported path.** Rejected: it is the
  dashboard's API reached past a CSRF guard, it hands an agent every mutating
  dashboard RPC to do one read, it breaks silently the day that surface gains
  auth, and it pages the whole table to filter in the client work the SQL
  `WHERE` clause should do.
- **Default-exclude `pod.*` from results, or add an `eventType` filter.** The
  issue's own suggestion, and rejected on tracing: both are permanent tax at
  every reader to hide a write nobody wants. A silent default is worse than
  the tax — a tool that drops 80% of the table without saying so is what
  someone debugs at 3am.
- **A new dedicated chronological read RPC.** Rejected: `SearchJournal` is
  already the agent-facing path; two appended proto fields beat a second RPC
  with its own client, its own handler and its own drift.
- **A numbered migration for the row deletion.** Rejected: it is one-time
  cleanup, not schema, and a migration whose `down` cannot restore what its
  `up` removed is a version step that lies about being reversible. Keeping it
  out also means the append-only property below stays literally true.
- **Keeping the rows and shipping only the parameters.** Rejected by the
  filter reasoning above.

## Consequences

- Pod phase *history* now lives only in Loki, which is retention-bound.
  `sessions.pod_phase` keeps the current value. Anyone who wants durable phase
  history should put it in observability, not in the journal.
- **`knowledge_journal` is still append-only** (`docs/DECISIONS.md`): no code
  path in the fleet deletes from it, before or after this change. The one-off
  `DELETE` is a human operation on a past mistake, deliberately not encoded as
  a fleet capability.
- The manual `DELETE` is irreversible — those rows exist nowhere else.
- Ordering must be run correctly: each `DELETE` only sticks once the build
  that stopped the corresponding write is live. An older `core` still journals
  `pod.*`, and an older `worker` still journals `session.*`, refilling the
  table behind whoever ran it.
- **The journal is now only what a session chose to write** — `agent_note` via
  `journal_write`, and nothing automatic. 98 rows became ~19. If that turns
  out to be too little, the fix is asking a session to write a real closing
  note, not reinstating a machine-generated one nothing read.
- `limit` is now capped at 500, because an unbounded read of the whole table
  became expressible in one call for the first time. That cap bounds rows,
  not bytes: the first live all-repo call returned 75 KB and blew the context
  budget, so the MCP handler also caps the serialized result at
  `maxJournalBytes` and says so when it trims (ADR-0046).
- `Store.Search` and `coreclient.SearchJournal` take options structs.
  `Since`/`Until` are adjacent same-typed values — the exact shape of the
  `SaveAgentSessionId` swap in the repo's trap list, which compiled, linted and
  passed every mocked test while breaking every resume.

## Out of scope

Blocking worker-pod access to `DashboardService` — issue #200. It is a
NetworkPolicy or authz change with its own blast radius, and holding a
read-path fix behind a security redesign helps nobody.

## Verification

- `core/internal/journal` had **zero** tests; nothing had ever exercised the
  FTS SQL or the `repo == ""` branch. Now covered against a real Postgres
  (`dbtest`, `-tags=integration`): window bounds inclusive/exclusive, the
  empty-query chronological path, all-repo versus scoped reads, the ranked
  path still ranking *and* respecting the window.
- `TestApplyPodEvent_WritesNoJournalRow` pins the deletion, so the append
  cannot come back looking harmless in review.
- `TestSearchJournal_DateOnlyUntilCoversThatWholeDay` was checked the way a
  guard test has to be: it was run against the *unfixed* code and observed to
  fail, not merely observed to pass against the fixed code.
- Live confirmation after the manual `DELETE`: the journal went from 468 rows
  to 96 — `session.started` 58, `agent_note` 19, `session.stopped` 12,
  `session.failed` 7 — and **zero** `pod.*`.
- End-to-end from a worker pod on the deployed `4.1.0`, which is the check
  this repo's trap list insists on and which no unit test substitutes for: one
  call, no `repo`, no `query`, `since` seven days back → **98 entries across
  five repos**, newest-first, zero `pod.*`. Entries timestamped after the
  `DELETE` confirm the deployed `core` had stopped writing them.
- That same call is what found the missing byte cap. It is worth recording
  that the feature's *first real use* surfaced a defect every green test had
  missed, and that the defect was a direct consequence of this ADR's own
  decision: relaxing a required parameter removed a bound nobody had noticed
  it was enforcing.
- **Not claimed here:** the `session.*` half exercised live. It ships with
  the next worker image; the check is a session ending normally and adding no
  journal row, and a crashed session's error appearing on the transcript
  instead.
