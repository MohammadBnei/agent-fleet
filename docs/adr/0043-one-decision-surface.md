# ADR-0043 — One decision surface: the dock, on both form factors

- **Status:** Accepted
- **Date:** 2026-08-13
- **Supersedes:** [ADR-0042](0042-console-rewrite.md) §5 (the desktop decision
  spine)

## Context

ADR-0042 §5 gave desktop a decision spine and mobile a docked decision. In
practice desktop ended up rendering a pending permission in **three** places at
once:

1. inline in the feed, as an answerable `PermissionCard`/`QuestionCard`/`PlanCard`
   (`SessionFeed` was mounted without `dockPendingDecision`),
2. as a `pending` item in the spine — a jump target, not answerable,
3. as an answer-chip row above the composer, for the single-question case
   (`chipQuestion` in `TaskDetail`).

Three renderings of one decision is three things to scan and two of them
can't be acted on. It also made "where do I answer this" a per-screen-position
question rather than a fixed one.

The spine's *other* job — the session's decision history, zoomed out — turned
out to be already covered. `feedVisibility` maps desktop's third density mode
to "tier-1 cards and alarms only", which is the same set the spine derived,
rendered larger, in context, and answerable. The spine was a second, weaker
implementation of the DENSITY control sitting next to it.

Mobile had the inverse problem: it docked the decision correctly but hand-rolled
its own flattened copy of all three cards (~115 lines inside
`MobileTaskDetail`). That copy could only dock a permission or a *single
non-multi-select* question, so a multi-question `AskUserQuestion` fell through
to the feed — the one place ADR-0042 says a blocking decision must not be, since
it scrolls away. It also had no reason field, so a denial from a phone was
always the canned string `"use the fixture"`.

## Decision

**One pinned dock, one shared component, both form factors.**

- `components/DecisionDock.tsx` renders the pending decision — permission, plan,
  or question — pinned between the feed and the composer. Both detail views pass
  `dockPendingDecision` to `SessionFeed`, so the feed never renders it a second
  time; the feed keeps rendering *resolved* decisions, which is its job.
- The dock renders the **same** `PlanCard`/`PermissionCard`/`QuestionCard` the
  feed uses. It is a pin, a scroll bound (`max-h-[45vh]`) and an edge — not a
  fourth rendering of a decision.
- **The decision spine is deleted**, along with `spineItems`/`SpineItem`
  (~95 lines of derivation) and `jumpToEntry`. `DENSITY → decisions` is the
  zoom-out. `↓ next decision` goes with it: a pinned dock is always on screen,
  so there is nothing to jump to.
- The desktop `chipQuestion` row above the composer is deleted — the dock
  renders the real card there now.
- `hasPendingDecision(entries)` replaces mobile's three bespoke derived
  values (`pendingPermission`/`dockQuestion`/`docked`).

A permission outranks a question when both are somehow open, since a permission
blocks the agent's own tool call.

## Consequences

- A decision is answerable from exactly one place per screen, always the same
  place, never scrollable-away — on both form factors.
- Mobile gains what the hand-rolled dock couldn't do: multi-question and
  multi-select `AskUserQuestion`, `ToolInputView`'s real diff for `Edit`/`Write`,
  and a free-text denial reason. `MobileTaskDetail` loses ~115 lines.
- `docs/adr/0042` §5's first half no longer holds; its mobile half is now the
  behaviour on *both* form factors.
- The `id="entry-<seq>"` anchors `SessionFeed` puts on rows are kept. Nothing
  jumps to them today, but they cost one attribute and are the correct hook if
  deep-linking to an entry is ever wanted.
- Lost with the spine: the `N resolved · N open` counter and the at-a-glance
  alarm count. Alarms are tier 5 in the feed and never gated by density, so
  they remain visible; the counter had no action attached to it.

## Verification

Local stack (throwaway Postgres + `core` + dashboard), seeded with a pending
`Bash` permission, a pending two-question `AskUserQuestion`, and a diff-shaped
`Edit` permission:

- desktop: exactly one `allow`, one `deny`, one `PERMISSION REQUEST` header and
  one reason input on a blocked session; no `DECISION SPINE`, no
  `↓ next decision`; the dock sits outside the feed's scroll container while the
  feed still scrolls
- deny-with-reason round trip: dock disappears, the reason reaches the feed, the
  resolved card reads `denied`
- mobile 390px: the two-question card docks (the old dock rendered nothing for
  this shape), dock bounded to 45vh and scrollable, tab bar and composer still
  visible, no horizontal overflow
- contrast/size audit re-run over the new dock surfaces (including the diff's
  red/green row backgrounds) in both themes: 272 nodes, worst 4.69:1, smallest
  12.1px, zero failures
