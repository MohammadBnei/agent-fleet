# ADR-0004: Claude Code subscription OAuth, not a metered API key

**Status:** Accepted
**Date:** 2026-07-30

## Context

`design-v0.md` §8's task envelope included `budget: { max_tokens, max_usd
}` with `max_usd: null` deliberately for v1 — "tracked and visible via the
ledger, not enforced, until a usage baseline exists." That envelope
assumed metered API billing (an Anthropic API key routed through a model
gateway) as the cost model, where a real dollar cap would eventually make
sense.

The actual implementation authenticates via `CLAUDE_CODE_OAUTH_TOKEN`
(minted with `claude setup-token`) — Claude Code's own subscription
auth, not a metered per-token API key.

## Decision

No `maxBudgetUsd` is ever set on `query()` calls in
`worker/src/planning.ts`. The Agent SDK still computes and reports
`total_cost_usd` in its result messages (logged via `logResult` into
`knowledge_journal`), but under subscription auth that figure is notional
— useful for relative comparison between tasks, not a real per-task charge.

## Consequences

- No budget-cap plumbing needed anywhere in this fleet — the entire
  `design-v0.md` §8 `budget.max_usd` enforcement question is moot as long
  as subscription auth is the auth model.
- `total_cost_usd` in logs/journal entries should be read as a relative
  signal (which tasks burned more turns/complexity), not a bill.
- If the fleet ever switches to metered API billing (e.g. to run many more
  concurrent workers than one subscription reasonably supports), this ADR
  must be revisited and real cost-cap enforcement designed — not silently
  assumed to still be moot.
