# ADR-0082 — Decision cards: only an explicit address wakes an agent, and the owner decides on cards

- **Status:** Accepted (owner-approved 2026-10-28; taint dropped by the owner 2026-10-29)
- **Built:** rollout slice 1 in `core/internal/dispatch` (`gate`, `addresses`, `Render`, `oneLine`): the address-only
  wake and the bot's own events dropped. Until the bridge's slice (§12) delivers `labeled` events, a T0 label is
  accepted but never arrives, and an unowned subject is read from today's single `trusted` bit
- **Date:** 2026-10-28
- **Supersedes:** [ADR-0079](0079-provenance-ledger.md) (provenance ledger, never built)
- **Amends:** [ADR-0067](0067-shared-and-private-sessions.md) §3 (no taint),
  [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4 (routing is not a wake), §6 (the gate), §7 (no
  taint), [ADR-0072](0072-approval-gated-executor.md) §5 (no taint warning; approvals are cards),
  [ADR-0078](0078-core-on-the-bridge.md) §5 (the gate, closing) and §8 (the echo, the trust bit); bridge ADR-0060 §6
  (one `trusted` bit becomes two facts)
- **Related:** [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md),
  [ADR-0043](0043-one-decision-surface.md), [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md),
  [ADR-0064](0064-definition-pod-profile.md), [ADR-0066](0066-agent-service.md), [ADR-0068](0068-islands.md),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md), [ADR-0081](0081-error-keys-in-proto-errors-traced.md)

## Context

This is the baseline for all agentic work: agents draft decisions, and the owner decides with exactly the right
information in front of him. ADR-0079 tried to make the fleet's own GitHub echoes trustworthy with a ledger of every
write. It was heavy, automatic by default, and still rested on matching bot events. What the code does today:

- **The wake is trust, not address.** `dispatch.gate` wakes on any trusted `human` event whose login is in
  `actors.allow`, on any thread `route` sends it to through `owns` or `subscribes`. `Render` quotes the PR body or
  comment into the waking message, and `oneLine` strips control characters but not bidi or zero-width ones.
- **Taint is half a mechanism.** Core marks a thread tainted when it holds an untrusted or context-only entry
  (`Open`, `sessions.Store.Tainted`). `Publish` then sends the session's hand-offs as untrusted, and the island hands
  a tainted thread to admins only (ADR-0067 §3). The worker never reads it. Enforcing it fully (slice 1 of this ADR's
  first draft, #341: a tainted session may only read) made any `gh`, `curl` or browser read cage a session for good,
  its own CI checks included.
- **Decisions are scattered.** Permission asks pin a pod; plan approval, event proposals (`FoldEvent`) and ADR-0072's
  executor rows are separate shapes. `Principal.Admin()` requires `Origin == ""`, so an island is never admin.
  Core sends `frame-ancestors 'none'`.

## Decision

1. **Wake: only an explicit address wakes an agent.**
   - A GitHub label whose sender is the T0 login, or `@<definition>` in a T0-written comment or body, outside `>`
     quotes and code blocks; a fleet `PublishEvent`; a console or island open (T0 by sign-in).
   - `owns` and `subscribes` only route: what they match arrives as context, never as a wake. An event type alone
     never wakes.
   - The fleet bot's own GitHub events never wake: they are acked and dropped. That covers self-echo and
     `synchronize`/`reopened`.
2. **Who may wake.**
   - **T0**, the owner: the `actors.allow` GitHub login, and console or island platform-admins.
   - **T1**: any fleet session, through `PublishEvent`.
   - **T2**: a stranger in our repo. **T3**: anything in a foreign repo.
   - T2 and T3 never wake, and their @mentions are inert. Their text reaches an agent only as context of work T0 or
     T1 started, labelled with its author. An unowned subject (a stranger's or a fork's PR or issue) arrives with its
     title, body and diff as context even when T0 addressed it: the wake message holds only T0 bytes plus references.
3. **No taint (owner, 2026-10-29).** What a session reads changes nothing about what it may do or whom it may wake.
   - Sessions keep today's tools and modes: `auto` answers everything but `rm`/`sudo` (ADR-0053); `gh`, `curl`,
     `wget`, `git fetch`, `WebFetch`, `WebSearch` and the browser stay available.
   - An entry keeps its author and tier as a label, for the reader and for a card's `outside_inputs`, never as a
     gate.
   - Core's taint goes: `Open`'s `Untrusted`, `sessions.Store.Tainted`, `Publish`'s trust bit (a fleet hand-off
     wakes), the island's admin-only tainted thread (ADR-0067 §3) and ADR-0072's tainted-step warning.
   - What bounds a stranger's words: they never start work (§1, §2), a card option carries only its bound effect
     (§6), and the default branch moves only through the merge right (§9) or an owner card.
4. **An agent drafts a card when the choice is the owner's**: a stranger's PR, a step outside its mandate, a
   decision with real options. It uses a `draft_card` sidecar tool. Permission asks stay as they are (ADR-0029).
5. **The card** is a durable question entry in the proposals inbox, never a `permission_request`, so it never pins a
   pod.

   | Field | Filled by |
   |---|---|
   | `id`, `state` (`open` \| `answered` \| `expired` \| `stale`), `expires_at` | core |
   | `writer {definition, session}`, `subject {repo (full `owner/name`), kind (`pr` \| `issue` \| `alert` \| `repo`), number (none for `repo`), url}` | core |
   | `ask`: at most 280 chars | agent claim |
   | `pinned {head_sha, sources [{object_id, sha256}], open/closed}` | core |
   | `outside_inputs [{author, tier, object_id, url, text}]`, verbatim | core |
   | `verified [{fact, value, checked_at}]`: CI status, diff stats, PR author, head repo | core |
   | `options` [2..3] `{id, label (≤ 80 chars, agent claim), effect {verb, repo, number, head_sha, path?}, reversible yes\|partial\|no, recommended}` | agent, effect checked by core |
   | `write_own: bool` | agent |
   | `answered_at`, `answered_by`, `answer {option_id \| own_text}`; kept after the card closes | core |

   Lengths are bounded in the proto, so every surface can rely on them: `ask` 280, `label` 80, `outside_inputs.text`
   2,000 (longer is cut, and the card says so, with the source's url).

   **API** (`AgentService`): `ListCards` filters by state (any set of the four), `subject.repo`, newest first, paged;
   answered, expired and stale cards stay listable as history. `GetCard`; `AnswerCard {card_id, option_id |
   own_text}`; `WatchCards`, a stream that sends each changed card id and a list version, so surfaces never poll.

   Rendering: plain text, every href shown, bidi and zero-width characters stripped, lengths bounded, agent text
   labelled "agent claim" and set apart from what core filled, the recommendation marked but never pre-selected. On a
   phone: the ask, 2–3 buttons, an "N outside inputs" chip and a stale badge.
6. **Choosing.**
   - An option authorizes only its bound effect. Core runs the effect, or hands it to the writer's thread as a T0
     message from a fixed template: "owner chose option k of card c: <effect>".
   - Agent prose passed on stays labelled "agent claim, owner-chosen", never the owner's words. Only write-your-own
     text is T0.
   - Core re-checks the pinned state when the owner answers and again at hand-off. On a mismatch the card goes stale
     and must be re-drafted.
   - Error keys (ADR-0081): `card.stale`, `card.expired`, `card.fresh_auth_required`.
7. **Answering.**
   - Only `Admin()` answers: in the console, in an island, or in another of our app surfaces (the mountain 3D game),
     provided the answer call goes from the browser straight to core under core's own OIDC session, never through
     another app's server or credential.
   - Island and embedded surfaces: core's `frame-ancestors` lists only our app origins; a confirm step re-shows the
     effect as core renders it and needs a second deliberate action; an irreversible effect needs a fresh sign-in once
     the session is older than 10 minutes.
   - A surface with its own UI (the game) calls the card methods cross-origin with credentials. Core's CORS lists
     only configured origins (`https://game.bnei.dev`; `http://127.0.0.1:7410` only in a local `--dev` core). Sign-in
     is a top-level redirect to core with a return URL limited to those origins. `card.fresh_auth_required` carries
     that sign-in URL.
   - Since that surface draws its own confirm step, a script injected into it could answer for the owner. So an
     irreversible effect answered from such a surface always passes core's own top-level confirmation page, whatever
     the session's age. A reversible one may be answered in place.
   - Creating a card sends a notification. An expired card closes as "no action": ignoring never approves.
8. **Load.**
   - At most one open card per subject; later inputs fold into it (`FoldEvent`).
   - A daily card budget per definition, with a digest card for the overflow. Cards count toward the thread's wake cap
     (`MaxAgentWakes`).
   - Plan approval, "Close threads" and ADR-0072 executor approvals become cards.
9. **Merge right.**
   - Approval by a fleet review agent lets the writer agent merge exactly that head. The approval is a core record (a
     `PublishEvent` such as `review.approved` pinned to `head_sha`), not a GitHub review: every agent shares the bot
     identity, and GitHub refuses self-approval.
   - On approval core posts an `agent-review` commit status; each repo's ruleset requires it on the default branch. A
     push after approval voids it.
   - A stranger's PR merges only through an owner card.
   - So the default branch holds only content written and approved by fleet agents, or chosen by the owner.
10. **Closing** (kept from ADR-0079): a human close closes the threads; a fleet merge into the default branch closes
    them; any other fleet close becomes a "Close threads" card. Capacity is unchanged: a writer and a reviewer need
    two sessions at once, and `MAX_EXTERNAL_SESSIONS=1` (ADR-0078 §7) makes the second a card until a deploy raises it.
11. **Dropped from ADR-0079:** the provenance ledger, `ForgeWrite` as a trust mechanism, echo matching and the intent
    wait.
    - **Kept, narrowed (owner, 2026-10-29):** a GitHub write path through the provisioner, which holds the App key,
      for exactly two callers: running a chosen card's bound effect and posting the `agent-review` commit status.
      The provisioner mints a token per repository with only the permission that write needs. Core still holds no
      GitHub credential, and the path records nothing about trust.
12. **Bridge side** (a dependency on `ukubi-bridge`; not changed here; amends bridge ADR-0060 §6):
    - deliver `pull_request.labeled`, `issues.opened` and `issues.labeled`;
    - send `actor_trusted` (the addresser is T0) separately from `thread_owned` (an owned, same-repo subject), in place
      of today's single `trusted = Trusted[actor] && owns(author)`.

## Alternatives considered

- **Taint with read-only enforcement** (this ADR's first draft; slice 1 built and reviewed as #341, closed unmerged).
  Rejected by the owner, 2026-10-29: it caged ordinary work (checking one's own CI tainted a session for good) to
  guard a path the address-only wake already closes for strangers.
- **Taint as a hand-off bit only** (a session holding a stranger's text cannot wake others). Rejected by the owner,
  2026-10-29: hand-offs between our own sessions stay free, and the wake gate is the boundary.
- **Trust the bot's events.** Its own pushes and comments would wake loops on the fleet's own writes; an explicit
  address is enough.
- **ADR-0079 as written.** Heavy (a ledger, four tools, a write path, an echo wait), and automatic by default.
- **Wake by humans only.** Kills agent hand-offs.
- **One GitHub App per definition.** Hand-made Apps, and still per definition, not per session.
- **The owner's click makes agent prose T0.** That launders through the click.
- **A slim ledger.** Once bot events never wake, trust does not need one.

## Consequences

- Strangers' words never start work, but they can steer a session the owner or the fleet started, and that session
  can hand off. The owner accepts this (2026-10-29); bound card effects and the merge right are the checks left.
- A rubber-stamped option still carries only its bound effect.
- Card volume must be measured before this is called done.
- Code that changes: `dispatch.gate`, `route` and `Render`; core's taint (`Open`'s `Untrusted`,
  `sessions.Store.Tainted`, `Publish`'s trust bit, the island's tainted-thread path); `Principal.Admin()` and
  `frame-ancestors`; the proposals store (a card kind) and the console's decision UI (ADR-0043's dock shows cards).
- Core has no GitHub write path (ADR-0078 §5) and holds no GitHub credential (ADR-0075). Card effects and the
  `agent-review` status go through the provisioner, which holds the App key.

## Rollout

Slice 1 is built. Slices, in order:

1. The address-only gate, and dropping the bot's events.
2. The card store, the `AnswerCard` API, `draft_card` and the console UI.
3. Removing core's taint.
4. The bridge's events and trust facts.
5. The merge right: the `agent-review` status and the rulesets.
6. Island and embedded surfaces, with `frame-ancestors`.
7. Folding the old decision kinds into cards.
