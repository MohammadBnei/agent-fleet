# ADR-0067 — Shared and private sessions, authorship, and the definition's tool surface in the worker

- **Status:** Proposed
- **Date:** 2026-10-20
- **Related:** [ADR-0066](0066-agent-service.md) (the AgentService and its
  authz table, which reads the rules here), [ADR-0062](0062-defined-agents.md)
  §7 (the spec fields still inert after P3), [ADR-0063](0063-context-packs.md)
  §6 (`settingSources: ["user"]` needs another way to load skills),
  [ADR-0064](0064-definition-pod-profile.md) (P1 did not bound the tool
  surface), [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md),
  [ADR-0052](0052-auto-mode-and-the-bypass-launch-profile.md),
  [ADR-0060](0060-claude-codes-own-cross-session-channel-is-open.md),
  [ADR-0070](0070-headless-invoke.md), ADR-0071 (#257, taint and
  context-only follow-ups).

## Context

ADR-0066 lets non-admins and machine clients reach definition sessions. Two
things it needs do not exist (verified on main at 7c6f1a5):

1. **Sessions have no owner.** `sessions` has no owner, visibility or
   principal column (migrations 000001 to 000012). `transcript` rows record
   `"from"` (`human`, `agent`, …) but not which human. With one admin that
   was enough. With a shared `opus-workspace` thread, the agent and the other
   members need to know who said what, and a private thread needs an owner to
   check against.
2. **The worker does not apply the definition.** It gets no spec
   (`GetSessionResponse` carries only the `Session`, `core.proto:329`). It
   passes a fixed `allowedTools` list (`worker/src/session.ts:851-867`), and
   the SDK runs those tools **without** `canUseTool`. It loads the target
   repo's `.claude/settings.json` (`settingSources: ["user","project"]`),
   whose allow rules also skip `canUseTool`. `tools`, `thinking`,
   `systemPrompt`, `limits.maxTurns` and `model.allow` reach nothing
   (ADR-0062 §7, ADR-0064). Today the only boundary is that every caller is a
   platform-admin. That stops being true with ADR-0066.

There is also a delivery gap that ADR-0071's context-only follow-ups fall
into. An entry appended to a cold session is never delivered, because the
next pod's cursor starts above it (`server.go:955-996`). An entry appended to
a live session is streamed by `StreamHumanMessages`, and the worker starts a
turn on it (`session.ts`, the `input.push` loop). Neither is "read at the
next wake".

## Decision

### 1. Every AgentService session has an owner and a visibility

Migration 000015 adds to `sessions`:

| Column | Meaning |
|---|---|
| `owner` | the member's OIDC `sub` (hashed, ADR-0066 §2) or `client:<name>` |
| `visibility` | `shared` or `private` (CHECK) |
| `invoker` | the client name, for client-opened sessions |
| `interaction` | `ask` or `deny` (CHECK) |
| `lifecycle` | `resident` or `single-shot` (CHECK) |
| `thread_key` | the client's thread key (ADR-0066 §6) |
| `cold_from_seq` | the first context-only entry appended while the session had no pod (§3) |

It also adds:

- a unique partial index on `(invoker, definition_name, thread_key) WHERE
  archived_at IS NULL AND thread_key IS NOT NULL`. ADR-0071's draft adds the
  same column with a `(definition_name, thread_key)` index; this migration
  replaces that, and 0071 references it;
- the `client_requests` table, `sessions.pod_reason`, and the unique
  `(session_id, reply_to_seq)` index for answers (ADR-0066 §4, §7, §10);
- a CHECK that `owner`, `visibility`, `interaction` and `lifecycle` are set
  only when `definition_name` is.

A member's `CreateThread` sets `interaction: ask` and `lifecycle: resident`.
Definition sessions created from the console (`DashboardService`) get the
admin's `sub` as owner, `interaction: ask`, `lifecycle: resident`, and
`private` visibility unless the admin chooses `shared`. **Existing rows keep
`NULL` owner and visibility, which read as admin-only.** P1 to P3 test
sessions never become visible to an audience. Their worker treats `NULL`
interaction and lifecycle as `ask` and `resident`.

"Open" is derived (`archived_at IS NULL`), never a stored state (ADR-0048
§2).

### 2. Who sees and who answers

| Session | Sees it | Posts messages, answers prompts |
|---|---|---|
| `shared` | every member of the definition's current audience group, the owning client, admins | the same set; a client only answers, and only with `interaction: ask` |
| `private` | the owner, admins | the owner, admins |
| `NULL` (pre-existing) | admins | admins |

**A tainted session (§3) is posted to and answered by admins only**,
whatever its visibility, so neither a member nor the client that fed it
untrusted input can approve a turn that input drove.

- Admins see and act on every session, in the console and through
  AgentService. The console stays the full mirror.
- A member chooses `shared` or `private` at `CreateThread`. A client must
  choose at `Open` (ADR-0066 §6).
- `SetConfig` and `Stop` are the owner's or an admin's, never every member's
  (ADR-0066 §4).
- **Audience applies to every member row, private ones included.** A member
  sees, answers or configures a session only while they are in the
  definition's current audience group (ADR-0066 §5). Dropping a group from a
  definition therefore cuts that group off from its shared threads **and**
  from its members' own private threads, at once. Admins and the owning
  client are not audience-bound.
- A session with `interaction: deny` has no prompts to answer (ADR-0070).

### 3. Authorship and trust are recorded by core, never parsed from text

`transcript` gains `author_sub`, `author_name`, `actor`, `causation_id`,
`context_only` and `untrusted` (both default `false`).

- **Author.** Core fills the author from the principal on every human-side
  append (message, answer, permission response, plan approval). An append
  from a member with an empty `sub` is refused. For a client the author is
  `client:<name>`, and `actor` is the actor `Open` was given (bounded by
  `actors.allow`, ADR-0066 §6).
- **The agent sees authors in a structured envelope.** The worker wraps each
  human message in a fixed envelope whose author attribute comes from the
  entry's `author_name`, escaped. It is never taken from the message text, so
  a member cannot speak as another member, and a display name cannot close
  the envelope.
- **Untrusted.** A `context_only` entry is untrusted. So is every message of
  an `Open` with `tainted: true`, which the client sets when the event's
  causation chain includes an untrusted event (ADR-0071 §7). On an existing
  thread such a message is also forced `context_only`; only a thread's
  opening message can be tainted and wake (ADR-0066 §6).
- **`tainted` is derived:** a session is tainted when its transcript has any
  untrusted entry. It is not stored. The widget, the console and ADR-0072's
  approval buttons show it, and §2 restricts who answers.
- **Context-only entries reach the agent at its next turn and never start
  one:**
  - **Live pod.** `StreamHumanMessages` sends them flagged. The worker
    buffers them and never starts a turn on one. It prepends them, quoted
    and labelled untrusted, to the next turn's input.
  - **Mid-turn reads.** `WaitForMessages` (the `wait_for_messages` tool)
    returns entries with the same flag, and the sidecar renders a
    context-only entry with the same untrusted label, never as plain text.
  - **Cold session.** The append sets `cold_from_seq` if it is empty. The
    next `WarmIfIdle` gives the new pod a second cursor. Once the pod has
    acked its spec (ADR-0066 §8), core streams it the context-only entries
    from `cold_from_seq` up to its normal cursor, and only those (never a
    stale abort or permission response from that range), then clears
    `cold_from_seq`. A pod that fails before the ack leaves it set for the
    next warm. This is the upgrade path that `WarmIfIdle`'s own comment
    names.
  - When ADR-0066 §6 replaces an unwarmable thread, context-only entries
    still undelivered on the old thread are not carried over. The new
    thread's first entry links the old one, where they stay readable.
  - A worker that predates this ADR would start turns on them. It never
    receives any entry at all, because it never acks the `AgentSpec`
    (ADR-0066 §8).

### 4. The worker gets the spec and applies it

`GetSessionResponse` gains `AgentSpec`: the pinned row's effective `tools`,
`sidecarTools`, `model.allow`, `thinking.default`, `mode.allow`,
`systemPrompt`, `limits.maxTurns` and `output`, plus the session's
`interaction` and `lifecycle`. Repo sessions get no `AgentSpec` and keep
today's behaviour. The worker applies the spec, then calls `AckAgentSpec`,
and core streams it nothing before that (ADR-0066 §8). For definition
sessions:

- **`tools.allow` is decided inside `canUseTool`**, never passed as the SDK's
  `allowedTools` (ADR-0062 §7). `allowedTools` holds only the sidecar MCP
  tools in `sidecarTools`.
- **A call outside `tools.allow` is denied, not asked.** The definition is
  the ceiling. A member's click can never widen it, so a member cannot
  approve `curl` or a stray `git push --force` that the definition does not
  list.
- **Asking happens only inside the allow list.** A `Bash(<prefix>:*)` match
  still asks, because `:*` admits arguments such as `--upload-pack=…`, which
  run local commands (ADR-0062 §7). That holds until the worker has its own
  argument check for the rule.
- **An answer is `{behavior, message}` only.** The worker ignores
  `updatedInput` and `updatedPermissions` in a definition session's answers,
  as core already strips them (ADR-0066 §4).
- **`tools.deny` goes in flag-scope `permissions.deny`**, so it holds against
  any mode, including the calls `canUseTool` never sees (next point).
- **What `canUseTool` does not see.** In `default` mode the SDK does not call
  it for calls it judges safe: `Read`, `Glob` and `Grep` inside the working
  directory, and read-only Bash (`session.ts:877-882`). Those run even if
  `tools.allow` omits them, and even under `interaction: deny`. That is
  accepted: they read only the definition's own worktree, and the egress
  lock (ADR-0065) leaves no way to send what they read anywhere. A definition
  that must not read something lists it in `tools.deny`.
- **Modes.** `mode.allow` is limited to `default` and `plan` for
  definitions. The ceiling (`load.go`) already rejects `acceptEdits`, `auto`
  and `bypassPermissions` by name, because they skip `canUseTool`. **This
  ADR also removes `dontAsk`:** it never calls `canUseTool`, so with
  `allowedTools` holding only sidecar tools it would deny every edit and
  command, including the ones `tools.allow` lists. A definition that lists
  `dontAsk` fails validation, and its sessions stop warming until it is
  fixed. `permissionMode` is always passed (SDK 0.3.301 defaults to `auto`).
- **A mode change never approves anything.** Today a permission-mode change
  resolves the oldest pending call as allowed (`resolveOnePendingAllow`,
  `session.ts:727-736`). In a definition session it resolves nothing, so
  `SetConfig` cannot bypass the seq-bound answer rules or the "tainted:
  admins only" rule.
- **`settingSources: ["user"]`.** The target repo's `.claude/settings.json`
  (allow rules, hooks, env, enabled plugins) is never read. Memory still
  loads: the worktree is added to `additionalDirectories` next to `/genome`
  (ADR-0063), with `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`. Spike
  run 2026-10-20 (SDK 0.3.303, by ADR-0063's owner):
  - `<worktree>/.claude/CLAUDE.md` loads (typed Project), after
    `/genome/CLAUDE.md` and `/genome/.claude/rules/*.md`.
  - User-scope skills load: a skill under `CLAUDE_CONFIG_DIR/skills` is
    listed.
  - Skills under `additionalDirectories` do **not** load, for both
    `/genome/.claude/skills` and `<worktree>/.claude/skills`.

  So:
  - **Pack skills** come from a read-only mount of the session's genome
    skills at `/claude-home/skills`. That mount is ADR-0063's follow-up PR,
    by its owner, and it ships first (ADR-0066 rollout step 2).
  - **Repo skills and repo-enabled plugins do not load** in definition pods.
    Checked 2026-10-20: neither `MohammadBnei/notes` nor
    `opus-tech-hq/workspace` has `.claude/skills`. Both enable only the
    plugin `shared@shared-plugins`. It does not load today (the egress lock
    blocks its marketplace fetch) and will not load after this change.
    Anything a space needs goes into a pack or the definition.
- **`thinking.default`** maps to `effort: <level>` plus `thinking: {type:
  "adaptive"}` (plan spike 4). It is the definition's; there is no
  per-session thinking until the spec has a `thinking.allow` list.
- **`systemPrompt.append`**, **`limits.maxTurns`** and the session's model
  (checked against `model.allow` at create and at `SetConfig`) are passed to
  `query()`.
- `crossSessionInbound: "refuse"` and the `SendMessage`/`ListAgents` drop
  stay (ADR-0064 §2).

### 5. `SetConfig` stays within the definition

`SetConfig(model, mode)` is the owner's or an admin's. It checks each value
against the definition's `allow` list and the pinned row. Mode keeps today's
live path (`SetPermissionMode`). The model is stored on the session and takes
effect at the next warm. The response says which values are live and which
are pending, so the widget can show it.

## Alternatives considered

- **Keep `settingSources: ["user","project"]` and neutralise the repo's
  settings.** A flag-scope rule cannot remove a project allow rule; only ask
  rules outrank it (ADR-0052). Deleting the file in the clone shows up as a
  change the agent might commit, and `opus-workspace` pushes to `main`.
- **Pass `tools.allow` as `allowedTools`.** Shorter, and it removes the gate:
  the SDK runs `allowedTools` without `canUseTool`.
- **Ask for anything outside the allow list.** It was the first draft. It
  lets any audience member widen the definition with a click, which breaks
  "a definition only narrows".
- **Pre-existing sessions read as shared.** It was the first draft. It would
  have published every P1 to P3 test transcript to the audience on deploy.
- **A user table for authorship.** The cookie already carries `sub`, email
  and name. Storing them on each entry keeps history readable after a member
  leaves, with no join and no sync from authentik.

## Consequences

- A member in a shared thread sees what the other members wrote, by name.
  The agent knows who asked.
- **A Bash allow rule still asks** until an argument check exists. On
  `opus-workspace`, every `git push` asks a member once per call. That
  friction is accepted; an argument check for `git push` and `git pull
  --rebase` is the first follow-up.
- A definition that forgets a tool it needs fails visibly (denied), instead
  of asking someone. Its PR fixes it.
- A tainted shared session waits for an admin, even when a member is
  watching.
- Repo `.claude/settings.json`, repo skills and repo plugins stop applying to
  definition pods. Repo sessions are unchanged.
- The definition's tool surface becomes the pod's, which closes ADR-0064's
  "P1 does not bound the tool surface".
- An `ask` thread nobody answers holds its slot until `limits.timeout`, then
  is swept even with the permission pending (ADR-0066 §8). Admin definition
  sessions do not count against `limits.concurrency` or `MaxExternal`, so
  member and client threads never take their slots. The dispatcher treats
  the resulting refusals as routine (ADR-0066 §6). Event-driven definitions
  should set a short `limits.timeout`.
- Removing `dontAsk` from the ceiling changes a P1 rule. No definition on
  main lists it today (`workspace-base` allows `default` and `plan`).
