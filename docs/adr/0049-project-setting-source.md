# ADR-0049 — A session loads the target repo's own project settings

- **Status:** Accepted; the `permissions.ask` block's *placement* superseded by
  [ADR-0052](0052-auto-mode-and-the-bypass-launch-profile.md) (same list, same
  job, now `FLEET_ASK_RULES` in `worker/src/session.ts` so it can be omitted
  for a `bypassPermissions` launch). The `settingSources: ["user", "project"]`
  decision itself stands.
- **Date:** 2026-08-15
- **Resolves:** [ADR-0032](0032-fleet-shared-pvc-directory.md)'s open question
  ("Whether `settingSources` should ever include `"project"`")

## Context

`worker/src/session.ts` passed `settingSources: ["user"]`. That loads
`$CLAUDE_CONFIG_DIR` — the shared PVC that `git.Manager.SyncFleetShared`
mirrors `fleet-shared/`'s `CLAUDE.md`, `settings.json` and `skills/` into —
and nothing else. ADR-0032 named the `"project"` question and deliberately
left it open.

The consequence was larger than the skills gap that surfaced it. In the
vendored SDK's `cli.js`, `projectSettings` is what gates project `CLAUDE.md`
discovery:

```
if(wV("projectSettings")){ ...hb(A,"CLAUDE.md")... hb(A,".claude","CLAUDE.md") }
```

So a worker on `agent-fleet` never read `agent-fleet`'s own `CLAUDE.md` — not
the locked decisions, not the forbidden patterns, not the verification traps.
`fleet-shared/CLAUDE.md` has said "Target repo's CLAUDE.md wins for codebase
details" since it was written; that sentence described a file the session
could not see. Every worker has been running on the fleet-shared context
alone, plus whatever it happened to `Read`.

`.claude/skills/` is gated the same way (`case"projectSettings":return
".claude/skills"`), which is why this repo's own `fleet-ops` / `fleet-debug` /
`fleet-feature` skills were invisible to a session working on this repo.

## Decision

**`settingSources: ["user", "project"]`.**

`cwd` is `WORKTREE_PATH`, the session's clone root, so project discovery
resolves against the repo the session is actually working on.

**Not `"local"`.** That source is `.claude/settings.local.json`, which is
gitignored — its contents would reach a session without ever appearing in the
PR that landed them.

### The `permissions.ask` counterweight

`"project"` also merges the target repo's `.claude/settings.json`
`permissions.allow` into the session's rule set, and an allow rule removes
`canUseTool` from the path entirely — the same authority `allowedTools`
carries. A repo could ship `Bash(gh api:*)` in its own settings and approve
its own outward-facing commands.

`fleet-shared/settings.json` gains a `permissions.ask` block covering exactly
the bullets its README already listed as deliberately un-allowed: `git push`,
`gh`, `rm`, `sudo`, `kubectl`, `curl`, `wget`, `env`, `cat`.

This works because of the evaluator's ordering, read out of the vendored
`cli.js` (`Fw2`) rather than assumed:

```
if(Y[0]!==void 0) return {behavior:"deny", ...}   // deny rules
if(J[0]!==void 0) return {behavior:"ask",  ...}   // ask rules
...
if(X[0]!==void 0) return {behavior:"allow",...}   // allow rules
```

Rules are collected across every scope, and the function returns on the first
match — so a user-scope `ask` outranks a project-scope `allow` for the same
command.

`ask`, not `deny`: a human must still be able to approve a `git push`. Every
result is a PR, and approving the push *is* the review. `deny` would make the
fleet's own deliverable impossible.

### `Bash(curl http://127.0.0.1:*)` is removed, not kept

There is no specificity tiebreak in that ordering: a broad `ask` prefix
matches before a narrower `allow`. `Bash(curl:*)` in `ask` therefore swallows
`Bash(curl http://127.0.0.1:*)` and `Bash(curl http://localhost:*)`, which
would have left two allow entries in the file claiming a permission they no
longer granted. Both were deleted.

Localhost `curl` now prompts. That is the accepted cost: builds, tests and
installs are what run in a loop and they stay allowed, while `curl` is the one
entry whose un-prompted form is a supply-chain path — which is the reason the
README gave for never allowing it beyond localhost in the first place.

## Consequences

- A session gets its target repo's `CLAUDE.md` and `.claude/skills/`. For a
  session working on `agent-fleet` that is this repo's locked decisions and
  verification traps, which it previously had no access to.
- The fleet's authority ceiling is now stated in one file (`fleet-shared/
  settings.json`) instead of implied by what is absent from it. A repo can
  widen its *own* conveniences and cannot widen past the `ask` list.
- Adding to either list needs the other checked: a broad `ask` prefix silently
  swallows every narrower `allow` beneath it. The `curl` pair is the worked
  example.
- Onboarding a repo now means its `.claude/settings.json` is part of what runs
  in a worker pod. That file is reviewable in the repo; `settings.local.json`
  is not, and is excluded for that reason.
- Some of this repo's project skills (`fleet-ops`, `kind-local`,
  `dashboard-e2e`) drive `kubectl`/`ssh` against the live cluster, which a
  worker pod has no credentials for. They become discoverable and will fail if
  invoked — noise, not new authority.

## Verification

- `worker/src/session.test.ts` asserts `settingSources` equals
  `["user", "project"]` and does not contain `"local"`. Confirmed red against
  the old `["user"]` value before the change and green after — the assertion
  is on the value itself, not on the option being present.
- `bun run typecheck` clean.
- Live check on a kind cluster is what actually proves discovery, since
  `/dashboard-e2e`'s stub provisioner never mounts a real clone: a session
  targeting `agent-fleet` must resolve `/fleet-debug`, must answer a question
  about this repo's forbidden patterns from its CLAUDE.md rather than
  generically, and must still **prompt** on `env` — including when a
  throwaway `.claude/settings.json` in the clone allows it, which is the only
  direct test that `ask` outranks a project `allow`.
