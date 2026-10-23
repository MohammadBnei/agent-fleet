# ADR-0079 — A provenance ledger: what the fleet did outside, and who did it

- **Status:** Proposed
- **Date:** 2026-10-23
- **Amends:** [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §6 (the fleet's GitHub events are
  never trusted) and [ADR-0078](0078-core-on-the-bridge.md) §5 (closing) and §8 (its open question)
- **Related:** [ADR-0075](0075-github-app-identity-and-vault-credentials.md) (one GitHub identity, the provisioner
  holds the App key), [ADR-0064](0064-definition-pod-profile.md) (what a definition lease may call),
  [ADR-0072](0072-approval-gated-executor.md)

## Context

- **One identity.** Every fleet session acts on GitHub as `ukubi-fleet[bot]` (ADR-0075). The bridge marks the bot's
  events `actor=agent`, untrusted, and they reach threads `context_only`.
- **Taint makes automation dead.** Taint is ADR-0071 §7's mark that untrusted text has entered a session. An agent's
  own comment comes back as an echo and taints its own thread, so its later hand-offs never wake anyone (ADR-0078 §8).
  A review agent's comments taint the author agent the same way, so the code-review loop the fleet is for can never
  run unattended.
- **Trusting the bot is not the fix.** Every session shares the identity, so a session tainted by a stranger could
  launder text into a clean one (ADR-0071 §6).
- **What is missing is knowing which session wrote what.** No component sees it today: pods write with `gh` and git
  through the vault (ADR-0075 §7), and nothing in the pod can stamp a write without the agent being able to forge it.

## Decision

1. **A provenance ledger in core.**
   - Table `provenance`, one row per external object the fleet made:
     - its forge reference (`github`, kind `comment|review|pull_request|event`, the object id GitHub or the bridge
       returned), and its repo and number;
     - who made it: session, definition;
     - its trust, hops and causation when it was made.
   - **Only core writes it, and only for actions core performed.** It never records what an agent says it did.
   - `published_events` (ADR-0078 §8) folds into it as kind `event`.
   - Rows go with their session (`ON DELETE CASCADE`). A missing row means untrusted, which is the safe default.
2. **GitHub writes through core.**
   - Sidecar tools `github_comment` (issue or PR comment), `github_review` (`COMMENT`, `APPROVE`, `REQUEST_CHANGES`,
     with a body) and `github_pr` (open a PR from a pushed branch).
   - They call `CoreService.ForgeWrite`. Core checks the grant (the pinned spec's `sidecarTools`, like
     `publish_event`) and that the target is the session's own repo, computes the writer's trust and hops with
     `PublishEvent`'s rules, and calls the provisioner's new `ProvisionerService.ForgeWrite`. The provisioner mints a
     token for that one repository with the permission the write needs, makes the call, and returns the object id and
     URL. Core writes the row before answering.
   - The tools join `sidecarCeiling` and `PLAN_MODE_STILL_ASKS`. The worker sends them through `canUseTool`.
   - Direct writes stay possible: `gh` through the vault still works. Their echoes have no row, so they stay
     untrusted, as today. Nothing loses a capability; only the tool path earns trust.
3. **An echo carries its writer's trust.**
   - The dispatcher looks up every `actor=agent` GitHub event by its object id (`comment.id`, `review.id`, or the
     PR's id on `pull_request.opened`).
   - A match is handled as a hand-off from the writing session: actor `agent:<definition>`, the row's trust and its
     hops plus one, under the same gate (`actors.allow`, the per-thread wake cap, `MaxDepth`).
   - The writer's own thread never gets its own echo: an echo is acked for the thread of the session that wrote it.
     That ends the self-taint.
   - No match: untrusted, as today.
   - **Ordering.** The webhook can beat core's row. An unmatched `actor=agent` event younger than 15 seconds is
     retried, unsettled, like contention (ADR-0078 §5), before it counts as unmatched.
4. **Pushes stay untrusted.** Commits go through git, not the API, so a push has no row. A push echo renders only
   the PR's title and URL, which leaves little room for injected text.
5. **Closing: trust merges, verify closes** (amends ADR-0078 §5):
   - **Merged**, by anyone: the PR's threads close at once. GitHub's branch rules already had to allow the merge.
   - **Closed by a trusted person:** they close at once.
   - **Closed by the fleet's identity:**
     - The threads stay open, and core asks the bridge client's `closeVerifier` definition: it opens that
       definition's thread on the PR's subject with a core-written message naming the PR.
     - The verifier answers with a `confirm_close` tool (`close` or `keep`). Core takes `close` only from a session of
       the configured verifier, when it is untainted and not the thread's own definition, and only while the
       request is pending. It then closes the threads.
     - With no verifier configured, or no answer in 24 hours, the request becomes an inbox proposal of kind
       `close`. The console shows it as "Close threads", and accepting it closes them.
   - **Closed by anyone else** (a stranger closing their own PR): a `close` proposal.

## Alternatives considered

- **Drop the fleet's own events.** Simple, but agents could never act on each other's reviews through the bridge.
- **Count echoes as no taint.** It launders through the shared identity (ADR-0071 §6).
- **One GitHub App per definition.** Each App is created by hand, with its own key and installation, and the result
  is per definition, not per session: a tainted session could still launder into its definition's other threads.
- **A marker in each comment.** Anything the pod can write, the agent can forge.

## Consequences

- The code-review loop can run unattended: a clean reviewer's review reaches the author as trusted.
- Every GitHub write that should count goes through core and the provisioner, so a provisioner outage stops them,
  as it already stops warms.
- The provisioner gains a write path to GitHub on core's behalf. Core never holds a GitHub credential.
- An unmatched fleet-bot event waits up to 15 seconds at the head of the line.
- The console can later show "written by session X" from the ledger. ADR-0072's executor runs can record there too.

## Rollout

1. This ADR.
2. The ledger (migration), `ForgeWrite` on both services, the three tools, and `published_events` folded in.
3. The dispatcher's echo lookup and the close rules, with `closeVerifier`, `confirm_close` and the `close` proposal.
4. A definition granted the tools, end to end, once the cluster is up.
