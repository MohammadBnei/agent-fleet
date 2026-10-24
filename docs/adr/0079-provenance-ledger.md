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
  own comment or push comes back as an echo and taints its own thread, so its later hand-offs never wake anyone
  (ADR-0078 §8). A review agent's comments taint the author agent the same way, so a code-review loop between agents
  can never run unattended.
- **Trusting the bot is not the fix.** Every session shares the identity, so a session tainted by a stranger could
  launder text into a clean one (ADR-0071 §6).
- **What is missing is knowing which session wrote what.** No component sees it today: pods write with `gh` and git
  through the vault (ADR-0075 §7), and nothing in the pod can stamp a write without the agent being able to forge it.
- **What the bridge delivers** (`ukubi-bridge/internal/github`): `pull_request` opened, synchronize, closed and
  reopened, `issue_comment.created` and `pull_request_review.submitted`, with the full webhook body as data. Edits
  and deletions are not ingested.

## Decision

1. **A provenance ledger in core.**
   - Table `provenance`, one row per external object the fleet made through core:
     - its forge reference: `github`, kind (`comment`, `review`, `pull_request`, `close`, `event`), the object id
       GitHub or the bridge returned, the repo and number, and a sha256 of the body it wrote (and the head sha for a
       PR);
     - who made it: session, definition, thread key;
     - its trust, hops and causation when it was made.
   - **Only core writes it, and only for actions core performed.** It never records what an agent says it did.
   - A row is written as an **intent** before the call (with the caller's idempotency key) and completed with the
     object id after it. The intent makes a retry after a timeout reuse the write instead of posting twice, and
     tells the dispatcher an echo is on its way.
   - `published_events` (ADR-0078 §8) folds into it as kind `event`, with its 10-per-hour budget. Forge writes have
     their own: 20 per session per hour.
   - Rows go with their session (`ON DELETE CASCADE`). A missing row means untrusted, which is the safe default.
2. **GitHub writes through core.**
   - Sidecar tools:
     - `github_comment`: an issue or PR comment.
     - `github_review`: a `COMMENT` review with a body. GitHub refuses approving or requesting changes on a PR its
       own author wrote, and every fleet PR is the bot's, so approval stays a person's.
     - `github_pr`: open a PR from a branch of the same repository, at a stated `head_sha`.
     - `github_close`: close a PR.
   - They call `CoreService.ForgeWrite`. Core checks:
     - the grant (the pinned spec's `sidecarTools`, like `publish_event`);
     - that the target repo is the definition's `workspace.repo` or in its `owns.repos`, and for a PR, that the head
       is a bare branch name of that repo.

     Core then computes the writer's trust and hops with `PublishEvent`'s rules and calls the provisioner's new
     `ProvisionerService.ForgeWrite`. The provisioner mints a token for that one repository with the permission the
     write needs, checks that a PR's branch head equals `head_sha`, makes the call, and returns the object id and URL.
   - Past `MaxDepth` the write is not refused: it is recorded untrusted. Depth bounds wakes, not writes.
   - Authorization:
     - `ForgeWrite` joins `callerSessionOf`, `definitionMethods` and `sessionOnlyMethods` (never with the
       provisioner token), like `PublishEvent`;
     - the tools join `sidecarCeiling` and `PLAN_MODE_STILL_ASKS`, and the worker sends them through `canUseTool`.
   - Direct writes stay possible: `gh` through the vault still works. Their echoes have no row, so they stay
     untrusted, as today. Nothing loses a capability; only the tool path earns trust.
3. **An echo carries its writer's trust.**
   - **Lookup.** For an `actor=agent` GitHub event, before the gate decides, the dispatcher looks up
     `(github, kind, object id)`: `comment.id` on `issue_comment.created`, `review.id` on
     `pull_request_review.submitted`, `pull_request.id` on `pull_request.opened`. Only these creating actions match.
   - **Checks.** A match counts only when the webhook's body hashes to the row's, and for a PR, when
     `pull_request.head.sha` equals the row's and the head repo is the base repo (the bridge's fork rule, which the
     override would otherwise drop).
   - **Effect.** A counted match is handled as a hand-off from the writing session: actor `agent:<definition>`, the
     row's trust and hops, under the same gate (`actors.allow`, the per-thread wake cap, `MaxDepth`). The match is
     stored in the event's `dispatch_decisions` row, so a redelivery reuses it, and the message is rendered from it.
   - **The writer's own thread** (same definition and thread key) gets no copy: it is acked there. That ends the
     self-taint. Other threads on the subject still receive it.
   - **No match:** untrusted, as today.
   - **Ordering.** The webhook can beat the completed row. Only when an intent is pending for that repo, number and
     kind does the dispatcher wait for it, unsettled, for up to 15 seconds. No other event waits.
4. **The fleet's own pushes and reopens are not delivered.** A `synchronize` or `reopened` by the bot has no row and
   would taint every thread on the PR, the pusher's own included. These events are acked and dropped. An agent that
   wants a reviewer to look again says so through `github_comment`, which carries its trust.
5. **What trust covers.** A trusted echo vouches for what the event says, never for the code: the reviewer reads the
   PR's current code, and comments made outside the tools, with `gh`. Those reads do not taint (ADR-0071 §7 reads the
   transcript only). That limit is accepted; merges stay behind branch rules and people.
6. **Closing** (amends ADR-0078 §5; replaces the verifier first proposed):
   - **Closed or merged by a person** (`actor=human`, trusted or not): the PR's threads close at once.
   - **Merged by the fleet's identity into the repo's default branch:** they close at once. The default branch's
     ruleset had to allow it.
   - **Any other close or merge by the fleet's identity** (including a merge into another base):
     - matched to a `close` row, from a writer that was trusted when it closed: they close at once;
     - otherwise, a `close` proposal in the inbox. The console shows it as "Close threads", and accepting it closes
       them. A `reopened` event dismisses a pending one.
7. **Capacity.** An unattended loop needs two sessions at once, the writer's and the reader's, since the writer's
   pod is usually still live when its echo arrives. With `MAX_EXTERNAL_SESSIONS=1` (ADR-0078 §7) the second becomes a
   proposal. Raising it to 2 is a deploy decision.

## Alternatives considered

- **Drop the fleet's own events.** Simple, but agents could never act on each other's reviews through the bridge.
- **Count echoes as no taint.** It launders through the shared identity (ADR-0071 §6).
- **One GitHub App per definition.** Each App is created by hand, with its own key and installation, and the result
  is per definition, not per session: a tainted session could still launder into its definition's other threads.
- **A marker in each comment.** Anything the pod can write, the agent can forge.
- **A verifier agent confirming the fleet's closes.** It decides by reading the PR on GitHub, where anything written
  through the shared identity can steer it; an untrusted close would spawn its pod; and it takes the only external
  slot. The ledger's trust is a sounder rule.

## Consequences

- The code-review loop can run unattended: a clean reviewer's comments reach the author as trusted, and the author's
  reply reaches the reviewer.
- Every GitHub write that should count goes through core and the provisioner, so a provisioner outage stops them,
  as it already stops warms.
- The provisioner gains a write path to GitHub on core's behalf. Core never holds a GitHub credential.
- Agents see the fleet's own pushes only through a comment that announces them.
- The console can later show "written by session X" from the ledger. ADR-0072's executor runs can record there too.

## Rollout

1. This ADR.
2. The ledger (migration), `ForgeWrite` on both services, the four tools, and `published_events` folded in.
3. The dispatcher's echo lookup, the dropped pushes and reopens, and the close rules with the `close` proposal.
4. A definition granted the tools, end to end, once the cluster is up.
