# ADR-0042 — The dashboard becomes a console: full-width views, ranked feed tiers, and decisions answerable from the list

**Status:** Accepted
**Date:** 2026-08-12
**Supersedes:** the resizable-panel and dual-feed-toggle mechanisms introduced
alongside [0027](0027-arbitrary-permission-mode-and-command-palette.md); the
header-modal placement of scheduled audits from [0035](0035-thot-cluster-agent.md)

## Context

`docs/dashboard-spec.md` was written as input to a UI rewrite. Its §8 lists six
ways the existing dashboard failed at the one job it has — *across N sessions
running unattended, make it instantly obvious which one needs me, and why*. One
(the four competing status badges) had already been fixed by collapsing them into
the precedence-ranked `sessionBadge`. The other five had not:

2. **The feed had almost no visual hierarchy.** A `$0.42 · 7 turns` result line, a
   compaction marker and the agent's actual prose were all ~10px grey text. The
   spec ranks twelve entry kinds into five tiers; tier 1 and tier 4 rendered
   identically.
3. **Blocking cards didn't dominate.** The thing needing a human sat inline in a
   feed, and the list only said that *a* decision was waiting — never which. Every
   answer therefore cost a navigation and a read. A blocked session is stalled
   until someone clicks, which makes that latency the product's real cost.
4. **Density had no control.** A long session was thousands of lines with no
   zoom-out, no jump-to-next-decision, no collapse-all.
5. **Mobile was a port, not a design** — on the surface most likely used to answer
   a blocking question away from a desk.
6. **No fleet-level overview.** With five concurrent sessions there was no
   at-a-glance who-is-working/blocked/done, only a 320px list of rows.

Two design mockups (`Agent Fleet Console.dc.html`, `Agent Fleet Console
Mobile.dc.html`, Claude Design project `fcfa31dc`) answer all five as one
console in two form factors: a shared token set, five views, and a shared
three-way density control.

## Decision

### 1. List and detail are full-width alternates; the sidebar is gone

The permanent 320px task-list sidebar and the "rest of the herd" strip are
deleted. The list view is now rich enough to *be* the fleet overview those stood
in for (§8 item 6), and the detail view spends the reclaimed width on a decision
rail and a fixed panel column. `?task=`/`?view=` still carry the state — no
router, per [0013](0013-go-fleet-core-and-e2e-provisioner-rewrite.md)'s plan.

Mobile gains a persistent top bar and a bottom tab bar, both lifted into
`App.tsx` because they span all five views. Consequently `Worktrees`/`Files` lose
their `onBack` props: the tab bar is how you leave.

**Audits is promoted from a header modal to a top-level view.** It is where a
proposed run is approved, so it needed a place a human actually visits.

### 2. A pending decision is rendered, and answerable, in the list

This is the central move. `listSummary(entries)` widens the per-active-task
transcript fetch `App.tsx` already made for the todo bars, yielding the pending
permission, the pending question and the in-flight tool call. The NEEDS YOU card
renders the real thing — an `Edit` as a line diff with allow/deny and an optional
reason, a single-select question as one-tap chips — and calls
`RespondToPermission`/`AnswerQuestion` directly.

**No new RPC, and no new poll**: the fetch already existed, still scoped to
`ACTIVE_STATUSES` and therefore bounded by the fleet's concurrency cap.

A plan is deliberately *not* answerable from the list: deciding on one means
reading it, and a three-line preview that can be approved is worse than a link.

### 3. Five feed tiers get five treatments

`SessionFeed` is shared by both form factors — the single largest defence against
mobile drifting into a weaker port again:

| Tier | Kinds | Treatment |
|---|---|---|
| 1 demands action | permission, plan, question | full-width card; on mobile, the docked decision |
| 2 narrative | prose, human, cross-session, thinking | readable prose, generous leading, a dot / `❯` / `↘` marker |
| 3 tool activity | call+result pairs | dense rows grouped into one bordered block per consecutive run, with a count and collapse-all |
| 4 lifecycle | result, init, mode change, telemetry, hooks | a hairline rule: label left, summary right |
| 5 alarms | auth failure, model error, failed MCP server | an orange bordered bar, never a log line |

Tiers 1 and 5 ignore the density control entirely — they are the reason the
screen exists.

### 4. One density control, with a deliberate per-platform difference

`taskDetail.density` replaces `hideToolsInFeed` and `hideChangesInFeed`. The two
mockups disagree about the third mode, and `feedVisibility(density, isMobile)`
encodes **both** rather than bending one to fit:

| stored value | desktop label | mobile label | narrative | tools | quiet |
|---|---|---|---|---|---|
| `everything` | everything | all | ✓ | ✓ | ✓ |
| `narrative` | narrative | talk | ✓ | — | — |
| `decisions` | decisions | calls | — | desktop **—** / mobile **✓** | — |

Mobile's third mode is tool activity, because that is what a phone screen has
room to scan when checking on a run; desktop's is decisions only. A test asserts
the disagreement, so a later "simplification" can't silently unify them.

### 5. Desktop gets a decision spine; mobile docks the decision

`spineItems(entries)` derives a session's decision history from entries already
parsed — resolved allows/denies with their reasons, plan approvals, the pending
request, and alarms. Each item jumps the feed to the real entry (rows carry
`id="entry-<seq>"`), and a `↓ next decision` button answers §8 item 4.

On mobile the pending decision **pins above the composer** instead of sitting
inline, because an inline card scrolls away on a phone (§8 items 3 and 5).

### 6. The panel column is fixed; the resize machinery is deleted

`components/Panel.tsx`, `fitPanels`/`autoFit`, the sidebar drag, and six
localStorage keys (~120 lines) are removed in favour of a fixed 266px column
(`SessionPanels`), reused as a bottom sheet on mobile. Both mockups specify a
fixed column and nothing in the panels needed the height a drag could buy.

### 7. Two themes, from the mockups' own tokens

`herd` (dark, default) and `herd-light`, mapped onto DaisyUI's semantic names so
all existing components reskin untouched: `primary`=violet accent,
`error`=pink (**blocked** — the product's loudest state), `warning`=orange
(alarms), `success`=green, `info`=amber (in motion). 25 derived shades DaisyUI has
no name for (three hairline depths, the muted text ramp, each status colour's
tinted surface/border pair) are raw custom properties exposed through `@theme
inline`, so one class follows the theme switch.

Two things worth recording because they are easy to get wrong:

- **Tailwind's `rounded-*` scale is independent of DaisyUI's `--radius-*`.**
  Zeroing DaisyUI's tokens squares its own components but leaves every explicit
  `rounded-lg` in the codebase rounded. The scale is zeroed in `@theme` instead of
  editing ~200 class names; `rounded-full` is untouched, because the status dots
  really are circles.
- **The theme has to reach `body`, not just the app root.** Otherwise `body`
  stays transparent and overscroll shows the browser's white — a flash of the
  wrong theme. Caught by asserting the computed body background, not by looking.

The stored theme is applied by a pre-paint inline script in `index.html`: reading
it in a hook runs after first render, which flashes dark at every light-theme user
on every load.

### 8. A page-load failure is inline, never a modal

`Worktrees`/`Files`/`Audits` used the shared `ErrorModal` for load failures. Now
that the mobile tab bar is the only way out of a view, an unreachable provisioner
or object store meant a modal that covered the screen and blocked every route off
the page. A load error is information, not a decision: `InlineError` puts it on
the page with a retry. Found by Playwright, not by review — the modal silently
intercepted every subsequent click.

## Backend changes

Cheap additions only, and **no migration** — nothing here changes the schema, so
[0030](0030-single-source-schema-via-golang-migrate.md) stays untouched.

| Added | Note |
|---|---|
| `WorktreeInfo.path` | already parsed out of `%(worktreepath)` into a local and thrown away |
| `WorktreeInfo.dirty_files` | `git status --porcelain` count, through the existing per-repo mutex |
| `WorktreeInfo.size_bytes` | a `filepath.WalkDir` sum — affordable **only** because `ListWorktrees` has no poller: the page fetches on mount and on an explicit refresh, so this is paid per human action. If anything ever polls it, cache by `(path, mtime)` first |
| `pvc_total_bytes` / `pvc_free_bytes` | `syscall.Statfs` on the worktrees root; non-fatal, the meter is simply omitted when 0 |
| `RunScheduledAuditNow` | moves `next_run_at` to now and nudges the existing loop, so a manual run takes the identical `ClaimDue → CreateDeduped` path a scheduled one does — including landing as `proposed` and including the dedup that skips it while a previous run is open. Nothing here knows how to create a task |
| `RetryTask` | sets `pending` and resets `retry_count`. Retry was automatic-only, so `failed_permanently` had **no path back**: a task that died of an expired OAuth token stayed dead after the token was fixed. Guarded inside the `UPDATE` so two clicks can't double-dispatch |
| a UI caller for `QueryLogs` | the RPC and its Loki client already existed with no caller at all — the reason a session went quiet was reachable only with cluster access |

## Consequences

- **`sessionBadge` and its siblings are unchanged, deliberately.** §8 item 1 asks
  for the ranking to be kept and given more weight; that is the callers' job.
  `sessionBadge.test.ts` passing untouched is the proof it survived.
- The list view now shows a diff, so it reads denser. That is the trade the spec
  asks for: the blocked session is the payload.
- `ctx` in the composer is labelled **last turn**. The SDK reports usage per
  result and nothing sums it; a session-level number would be a lie.
- **Still not implemented, and why:** shared-file authorship (ADR-0031 makes the
  object key the filename verbatim, with no task id on the presign), cron
  schedules (`db/migrations/000006` defers them for want of a parser — the UI
  renders `every 6h` from `interval_seconds` rather than implying one exists), and
  per-audit cost/findings (there is no run-history table, only a single
  `last_status` overwritten each tick). All three appear in the mockups; the
  layouts simply don't carry the line.
- Mobile and desktop still have separate list/detail components, gated by the
  `useMediaQuery` mount check. That gate is load-bearing, not cosmetic: mounting
  both once meant two concurrent `StreamTranscript` subscriptions per open task.
  They now share `SessionFeed`, `SessionPanels`, `DecisionInline` and the four
  primitives, which is what keeps them honest.

## Verification

Nine Playwright specs at **1440×900 and 390×844** against the real local stack
(`/dashboard-e2e`): both themes on all five views, theme surviving a reload, a
blocked session answered inline *from the list* with the denial reason reaching
the spine, `RetryTask` on a genuinely `failed_permanently` task, all three
density modes on both form factors including the deliberate mobile/desktop
disagreement, spine-jump landing on the real entry, the mobile decision dock
sitting above the composer, and the tab bar reaching every view.

Overflow is checked by **measuring rendered rects**, ignoring boxes an ancestor
clips by design, plus a hard `documentElement.scrollWidth <= 390` gate — the class
of bug that once pushed a mobile column to 437px is invisible to `tsc` and to
lint. Console errors and page exceptions fail the run.

Two of those specs found real defects that review had missed: the load-error
modal trapping navigation (§8 above), and `body` never taking the theme.

The four new worktree fields and the PVC meter can only be exercised against a
real provisioner — see `docs/e2e-test-log.md` for the cluster run.
