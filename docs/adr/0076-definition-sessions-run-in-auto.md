# ADR-0076 — Definition sessions run in `auto`, and their allow list grows by remembered approvals

- **Status:** Proposed
- **Date:** 2026-10-20
- **Amends:** [ADR-0062](0062-defined-agents.md) (the mode ceiling: `auto`
  is no longer rejected for definitions),
  [ADR-0061](0061-sessions-launch-in-auto.md) (definitions are no longer the
  exception to the `auto` launch default),
  [ADR-0067](0067-shared-and-private-sessions.md) §4 (modes, and "a call
  outside `tools.allow` is denied, not asked"),
  [ADR-0066](0066-agent-service.md) §4 (the permission answer gains
  `scope` and `rule_form`)
- **Related:** [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md)
  (what `auto` means in the fleet), [ADR-0066](0066-agent-service.md) §4
  (who may answer a prompt)

## Context

A live test on 2026-10-20, with `dev-notes` and `opus-workspace` warmed on
v6.0.2, had both sessions stuck on Bash prompts in `default` mode. The owner
decided: **definition sessions launch in `auto`, like Claude Code**, for every
definition and every caller, not only for admins. Asked in follow-up, the
owner chose:

- **"Auto within the allow list"**: a call the definition allows never asks,
  and a call it does not allow is not silently run.
- **"Dynamic allow list, ask once and remember"**: a call outside the list is
  parked once. Whoever answers it can allow it for that call, for the rest of
  the session, or (an admin) for the definition, and later matching calls run
  without asking.

Two facts shape how this can be built:

1. **The SDK's `auto` would bypass the allow list.** ADR-0053 records that in
   the SDK's `auto` mode, "anything the SDK's own classifier allows never
   reaches the callback at all". A definition session run with
   `permissionMode: "auto"` would therefore run calls that `tools.allow` does
   not list, without `canUseTool` ever seeing them.
2. **The SDK's `default` mode reaches `canUseTool` for every call that is not
   auto-safe and not settled by an allow rule** (ADR-0067 §4: `Read`, `Glob`
   and `Grep` inside the worktree and the additional directories, and
   read-only Bash, do not reach it). A definition pod has no allow rules in
   any scope it reads: `settingSources: ["user"]`, and the provisioner removes
   fleet-shared's `settings.json` from every definition pod's claude-home
   (`grpcserver/server.go:278-281`). The fleet decides everything else.

ADR-0062 rejected `auto` for definitions by name because it skips
`canUseTool`. That remains true of the SDK's mode. It does not have to be true
of the fleet's.

## Decision

### 1. For a definition session, `auto` is the fleet's mode, not the SDK's

- **Mode ceiling.** `mode.allow` for a definition may hold `auto`, `default`
  and `plan`. `acceptEdits` and `bypassPermissions` stay rejected by name.
  `dontAsk` is removed, as ADR-0067 §4 decided; the worker PR that ships this
  ADR also removes it from `load.go`'s `ModeCeiling`, where it still is on
  main.
- **Default.** `workspace-base` sets `mode: {default: auto, allow: [auto,
  default, plan]}`, so every definition that extends it launches in `auto`.
  Definition sessions follow ADR-0061's launch default like every other
  session, and ADR-0062's "write the mode explicitly, never `auto`" is
  withdrawn.
- **The worker never hands `auto` to the SDK for a definition session.** A
  session whose mode is `auto` runs the SDK with `permissionMode: "default"`,
  so every call that is not auto-safe reaches `canUseTool`. The session's
  stored and displayed mode is still `auto`. Only the value the worker passes
  to the SDK differs. `plan` is passed as `plan`. Two code sites must apply
  the same mapping, or the session silently changes mode:
  - **The launch check.** At init the worker compares the SDK's reported mode
    with its own and saves the SDK's on a mismatch. For a definition session
    it must compare against the mapped value, or every `auto` session would
    be saved back as `default`.
  - **The live switch.** A `permission_mode` entry calls
    `q.setPermissionMode`. For a definition session it must pass the mapped
    value, or a live switch to `auto` would put the SDK into its real `auto`.
  Both get a test.
- **The `auto` confirm text** (ADR-0053's "only rm and sudo still come to
  you") is wrong for a definition session, where anything outside the allow
  list still asks. The console and the widget show a definition-specific text
  for it.

### 2. What the fleet answers in each mode

For a call that reaches `canUseTool` in a definition session, in order:

1. `tools.deny` (flag scope) has already refused it, in every mode.
2. **`Edit` and `Write` outside the worktree are denied**, whatever the allow
   list says, and so are paths under the worktree's `.git/` or `.claude/`.
   The worker resolves the path (symlinks included) before matching. A bare
   `Edit`/`Write` in `tools.allow` means "inside the worktree", never "any
   path". The per-session `CLAUDE_CONFIG_DIR` (read-write) and `/genome` are
   outside it, so an agent cannot write its own `settings.json`, git config or
   hooks and widen itself.
3. A sidecar tool listed in `sidecarTools`: allow.
4. A call that matches `tools.allow` or a grant (§3):
   - `auto`: allow, except a Bash call that `autoModeStillAsks` matches (`rm`,
     `sudo`, ADR-0053), and a Bash prefix match whose command fails its
     check. A git prefix uses the argument check (`gitArgsAreInert`,
     ADR-0067 §4). Any other prefix, including a granted one, needs only the
     shell-word check: no metacharacter, no expansion, no chaining. Those ask.
   - `default`: the same, except that an `Edit` or `Write` matched by
     `tools.allow` asks, as Claude Code's `default` does. One matched by a
     grant does not: a grant is already a human's approval.
5. A call no grant could ever cover (a tool outside the fleet's tool ceiling,
   §3) is denied, not asked. Anything else asks, with the grant choices of
   §3.
6. `ExitPlanMode` asks in every mode.

Under `interaction: deny` (ADR-0070), every "ask" above is a deny, and grants
cannot be made, because nobody answers.

ADR-0067 §4's "a call outside `tools.allow` is denied, not asked" is replaced
by step 5: it asks, and the answer can remember. The definition is still the
ceiling **without a human**: in `auto`, nothing outside the allow list and its
grants runs unattended, apart from the SDK's auto-safe calls (reads inside the
worktree and the additional directories, read-only Bash), which ADR-0067 §4
already accepts.

### 3. Grants: the allow list grows by remembered approvals

- **The answer carries a scope.** A permission answer is `{behavior, message,
  scope, rule_form}`. `scope` is `once` (today's behaviour), `session` or
  `definition`. `rule_form` is `exact` or `prefix`. ADR-0066 §4 still strips
  every other field.
- **Core writes the rule, never the answerer.** Core renders the rule from
  the parked request itself, never from text in the answer:
  - `Edit` or `Write`: `exact` gives that one file (`Edit(notes/a.md)`), and
    `prefix` gives its directory (`Edit(notes/**)`). Both are always inside
    the worktree (§2 step 2);
  - any other tool in the ceiling (`TodoWrite`): its bare name;
  - Bash `exact` gives `Bash(<command>)`, and is refused for a command that
    ends in `:*` or would otherwise parse as a prefix rule;
  - Bash `prefix` gives `Bash(<first word> <second word>:*)`, or `Bash(<first
    word>:*)` when the second word is an option. It is offered only when the
    command passes the shell-word check.

  The answer card shows the exact rule before the click. The rule is as wide
  as the card says and no wider, and a member cannot type a wider one. A
  `prefix` choice is deliberately wider than the one call, and it says so.
- **Bounds.** A grant never overrides `tools.deny` or §2 step 2. It names
  only tools in the fleet's tool ceiling (`load.go` `toolCeiling`: `Read`,
  `Edit`, `Write`, `Glob`, `Grep`, `Bash`, `TodoWrite`). Anything outside the
  ceiling (`WebFetch`, `Task`, Playwright) is denied by §2 step 5, because the
  egress lock leaves those tools nothing to reach. A Bash grant still goes
  through the `rm`/`sudo` check and its command check (§2 step 4).
- **Who may grant.** `once` and `session`: whoever may answer the session
  (ADR-0066 §4, ADR-0067 §2; a tainted session is admins only). `definition`:
  admins only, because it widens the definition for every session and every
  member.
- **Storage.** `session_grants(session_id, rule, granted_by, granted_at)` and
  `definition_grants(definition_name, rule, granted_by, granted_at,
  revoked_at)`, in the migration that carries ADR-0067's columns. Definition
  grants are listed in the console next to the definition, and an admin can
  revoke one there. They are live, unreviewed state, like live context packs,
  but kept apart from packs, which never carry permissions (ADR-0063).
- **Delivery.** `GetSessionResponse` carries the session's grants and its
  definition's live grants, **outside** the `AgentSpec` and its sha (ADR-0066
  §8), so a new grant does not stop delivery to a running worker. The worker
  that resolves a `session` or `definition` answer adds the rule to its own
  set at once. Other running sessions of that definition pick up a new
  definition grant at their next warm.
- **Revocation is not live.** A revoked definition grant keeps working in
  every running session of that definition until its next warm, and a
  session grant lasts as long as its session. That is accepted; an admin who
  needs it gone at once stops the sessions from the console.
- **Promotion to git.** A definition grant worth keeping is moved into the
  definition's `tools.allow` by a PR, then revoked. Nothing does that
  automatically.

## Alternatives considered

- **Run the SDK in `auto`.** That is the literal reading of "auto like Claude
  Code". It hands allow decisions to the SDK's classifier, which never consults
  `tools.allow`, so a definition would no longer bound what its agent does
  unattended. The owner chose "auto within the allow list" over that.
- **Deny outside the list, edit the list in the console only.** No prompts at
  all, but each new need costs an admin a trip to the console. The owner chose
  ask-once.
- **Let the agent extend its own list.** Nothing would bound it but
  `tools.deny`, the vault and the egress lock.

## Consequences

- `dev-notes` and `opus-workspace` run their edit-and-sync loop with no
  prompts: allowlisted tools plus the git argument check.
- A member can widen their own session, and an admin a whole definition,
  with one click. That is authority, and it is recorded per grant with who
  granted it.
- The SDK's read-only auto-safe calls still skip `canUseTool` in `default`, as
  ADR-0067 §4 already accepts.
- Rollout: the mode mapping, the `auto` ceiling and the `workspace-base`
  default ship with the worker's `AgentSpec` PR (ADR-0066 rollout step 2),
  with `once` as the only scope. The grant tables shipped in migration 000015
  (#289); the grant logic (rendering, storage, delivery, the worker's
  matching, the answer card's choices and the console list under Settings ›
  Grants) ships next, on DashboardService. Every DashboardService caller is a
  platform-admin, so `definition` scope is open to all of them there; the
  member rule applies when AgentService's member principal lands.
