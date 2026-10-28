# ADR-0082 — Decision cards: only an explicit address wakes an agent, and a tainted agent only drafts a card

- **Status:** Accepted (owner-approved 2026-10-28)
- **Built:** none of it.
- **Date:** 2026-10-28
- **Supersedes:** [ADR-0079](0079-provenance-ledger.md) (provenance ledger, never built)
- **Amends:** [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md) (`auto` does not reach a tainted session),
  [ADR-0071](0071-events-reach-sessions-through-the-bridge.md) §4 (routing is not a wake), §6 (the gate), §7 (taint),
  [ADR-0072](0072-approval-gated-executor.md) §5 (a tainted step is a card), [ADR-0078](0078-core-on-the-bridge.md)
  §5 (the gate, closing) and §8 (the echo); bridge ADR-0060 §6 (one `trusted` bit becomes two facts)
- **Related:** [ADR-0043](0043-one-decision-surface.md), [ADR-0064](0064-definition-pod-profile.md),
  [ADR-0066](0066-agent-service.md), [ADR-0068](0068-islands.md), [ADR-0075](0075-github-app-identity-and-vault-credentials.md),
  [ADR-0076](0076-definition-sessions-run-in-auto.md), [ADR-0081](0081-error-keys-in-proto-errors-traced.md)

## Context

This is the baseline for all agentic work: agents draft decisions, and the owner decides with exactly the right
information in front of him. ADR-0079 tried to make the fleet's own GitHub echoes trustworthy with a ledger of every
write. It was heavy, automatic by default, and still rested on matching bot events. What the code does today:

- **Taint does not stop anything.** `canUseTool` (`worker/src/session.ts`) never reads taint; the worker has no notion
  of it. A definition session's `allowedTools` are its `sidecarTools`, which skip `canUseTool` entirely, and
  `definitionDecision` runs whatever `tools.allow` or a remembered grant (`core/internal/grants`) matches.
  `opus-workspace` lists `Bash(git push:*)` with `git.branch: main`, and `gitArgsAreInert` lets `git push` run
  unasked. ADR-0053's `auto` answers everything but `rm`/`sudo`. A session tainted by a stranger can push to main.
- **Core does not refuse a tainted caller.** `dispatch.Publish` publishes for a tainted session (untrusted, but sent).
- **Taint means two things.** `Open` writes `Untrusted: contextOnly || tainted` (`core/internal/agentsvc/open.go`),
  so every context-only entry taints; `sessions.Store.Tainted` reads `untrusted` and `Publish` reads
  `context_only OR untrusted`.
- **The wake is trust, not address.** `dispatch.gate` wakes on any trusted `human` event whose login is in
  `actors.allow`, on any thread `route` sends it to through `owns` or `subscribes`. `Render` quotes the PR body or
  comment into the waking message, and `oneLine` strips control characters but not bidi or zero-width ones.
- **Decisions are scattered.** Permission asks pin a pod; plan approval, event proposals (`FoldEvent`) and ADR-0072's
  executor rows are separate shapes. `Principal.Admin()` requires `Origin == ""`, so an island is never admin, and the
  island tells a tainted thread's viewer to go to the console. Core sends `frame-ancestors 'none'`.

## Decision

1. **Wake: only an explicit address wakes an agent.**
   - A GitHub label whose sender is the T0 login, or `@<definition>` in a T0-written comment or body, outside `>`
     quotes and code blocks; a fleet `PublishEvent` from a T1 session; a console or island open (T0 by sign-in).
   - `owns` and `subscribes` only route: what they match arrives as context, never as a wake. An event type alone
     never wakes.
   - The fleet bot's own GitHub events never wake: they are acked and dropped. That covers self-echo and
     `synchronize`/`reopened`.
2. **Trust ladder.**
   - **T0**, the owner: the `actors.allow` GitHub login, and console or island platform-admins. He vouches for every
     byte he writes or pastes, quotes included.
   - **T1**: a fleet session into which core delivered no untrusted entry and which ran no outside-read tool call.
   - **T2**: a fleet session that did either.
   - **T3**: a stranger in our repo. **T4**: anything in a foreign repo.
   - T3 and T4 never wake, and their @mentions are inert. Their text reaches an agent only as T3 context, which makes
     that session T2. An unowned subject (a stranger's or a fork's PR or issue) arrives with its title, body and diff
     as T3 context even when T0 addressed it: the wake message holds only T0 bytes plus references.
3. **Taint by rule.**
   - A session is tainted by an untrusted entry, or by an outside read at the moment it runs: `gh` against issues,
     PRs, comments or CI; `curl`/`wget`; `git fetch`/`pull` of a non-default ref. `canUseTool` marks it.
   - `WebFetch` and `WebSearch` stay out of definitions (`DEFINITION_BUILTIN_TOOLS` already omits them) and leave
     repo sessions too (owner, 2026-10-29). `git fetch` and `git pull` join `FLEET_ASK_RULES`, so every outside read
     reaches `canUseTool`, which taints, whatever the SDK's own `auto` classifier or the target repo's allow rules say.
   - "Context-only" (it did not wake) and "untrusted" become separate: ADR-0071 §7 now reads "a session is tainted
     when it holds an untrusted entry", not a context-only one.
4. **T2 reads and drafts a card, nothing else.** Enforced twice.
   - **Worker:** a tainted session may use only `Read`, `Glob`, `Grep` and a `draft_card` sidecar tool. `tools.allow`,
     definition grants and the session mode (`auto` included) are ignored, and the sidecar tools leave `allowedTools`
     so `canUseTool` sees them. Taint is re-read on every call, since it can arrive mid-session.
   - **Core:** `PublishEvent`, `PromptSession`, `Expose` and `RequestService` refuse a tainted session.
   - A T2 session's permission asks become cards.
5. **The card** is a durable question entry in the proposals inbox, never a `permission_request`, so it never pins a
   pod.

   | Field | Filled by |
   |---|---|
   | `id`, `state` (`open` \| `answered` \| `expired` \| `stale`), `expires_at` | core |
   | `writer {definition, session, tier}`, `subject {repo (full `owner/name`), kind (`pr` \| `issue` \| `alert` \| `repo`), number (none for `repo`), url}` | core |
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
   - An option authorizes only its bound effect. Core runs the effect, or opens a fresh T0 session that replaces the
     tainted thread, from a fixed template: "owner chose option k of card c: <effect>".
   - Agent prose passed on stays labelled "T2, owner-approved", never T0. Only write-your-own text becomes T0.
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
   - Approval by a T1 review agent lets the writer agent merge exactly that head. The approval is a core record (a
     `PublishEvent` such as `review.approved` pinned to `head_sha`), not a GitHub review: every agent shares the bot
     identity, and GitHub refuses self-approval.
   - On approval core posts an `agent-review` commit status; each repo's ruleset requires it on the default branch. A
     push after approval voids it.
   - A T2 reviewer can only draft a card. A stranger's PR merges only through an owner card.
   - So the default branch holds only T1-vouched content: written by T1, approved by T1, or chosen by the owner.
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

- **Trust the bot.** Every session shares its identity, so a T2 session would launder into a clean one.
- **ADR-0079 as written.** Heavy (a ledger, four tools, a write path, an echo wait), and automatic by default.
- **Wake by humans only.** Kills agent hand-offs.
- **T2 works in plan mode.** Its side effects would still originate from a stranger's words.
- **One GitHub App per definition.** Hand-made Apps, and still per definition, not per session.
- **The owner's click makes agent prose T0.** That launders through the click.
- **A slim ledger.** Once bot events never wake, trust does not need one.

## Consequences

- The owner becomes the gate for anything outside-influenced. Clean agent loops run only through explicit addresses.
- A rubber-stamped option still carries only its bound effect.
- Card volume must be measured before this is called done.
- Code that changes: `dispatch.gate`, `route` and `Render`; `dispatch.Publish` and the `PromptSession`, `Expose` and
  `RequestService` handlers; `Open`'s `Untrusted`; the worker's `canUseTool`, `allowedTools` and
  `definitionDecision`; `Principal.Admin()` and `frame-ancestors`; the island's tainted-thread path; the proposals
  store (a card kind) and the console's decision UI (ADR-0043's dock shows cards).
- Core has no GitHub write path (ADR-0078 §5) and holds no GitHub credential (ADR-0075). Card effects and the
  `agent-review` status go through the provisioner, which holds the App key.

## Rollout

None of it is built. Slices, in order:

1. Taint enforcement in the worker and in core, first, because the hole is live. It includes taking `WebFetch` and
   `WebSearch` out of repo sessions and adding `git fetch`/`pull` to `FLEET_ASK_RULES`.
2. The context-only versus untrusted split.
3. The card store, the `AnswerCard` API and the console UI.
4. The address-only gate, and dropping the bot's events.
5. The bridge's events and trust facts.
6. The merge right: the `agent-review` status and the rulesets.
7. Island and embedded surfaces, with `frame-ancestors`.
8. Folding the old decision kinds into cards.
