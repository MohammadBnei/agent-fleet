# fleet-shared

The git-tracked source for every worker pod's shared Claude Code context —
implements docs/adr/0019 point 6 / docs/adr/0032 / docs/adr/0033.

The provisioner (`provisioner/internal/git.Manager.SyncFleetShared`) clones
this directory onto the shared workspace PVC and mirrors `CLAUDE.md`,
`settings.json`, and `skills/` into `$CLAUDE_CONFIG_DIR` before every worker
pod starts. `worker/src/session.ts` sets `settingSources: ["user"]`, so
Claude Code discovers everything here natively — no `session.ts` change and
no worker image rebuild needed to add a skill.

To add a skill: create `skills/<name>/SKILL.md` here and open a PR. The
next worker dispatch picks it up automatically.

Replaces the old image-baked `worker/skills/agent-fleet-planning/` plugin
(manual vendoring, no automated sync, required a full rebuild for any
change) — that was the exact problem this directory fixes.

Currently out of scope: a `plugins/` subdirectory (nothing today needs a
bundle beyond a plain skill — see docs/adr/0032's "Out of scope").

## `permissions.allow` is what makes builds un-prompted

Before docs/adr/0048 the un-prompted set was a property of *where* a command
ran: `run_command` executed in the e2e sandbox, which held no fleet
credentials and only the task's worktree, so it needed no prompt. That pod is
gone and everything runs on the session's own pod through `Bash`, which is
`canUseTool`-gated — so without an allowlist here, every `bun install` and
`go test` blocks on a human. That would make the fleet unusable rather than
merely slow.

The list is the same file and syntax a CLI user edits, which is the point: the
policy is legible and changeable by a human without a redeploy, instead of
being implied by a pod boundary.

What is deliberately NOT on it:

- **Anything outward-facing.** `git push`, `gh pr create`, `gh api` — these
  are the actions a human is agreeing to when they approve, and every result
  is a PR. Approving them is the review.
- **Anything destructive or unscoped.** No bare `rm`, no `sudo`, no
  `kubectl` (a cluster-access session reaches the cluster through the
  `thot-executor` shim, which is its own gate — docs/adr/0037).
- **`curl` to anywhere but localhost.** Fetching from the internet mid-build
  is how a supply-chain surprise arrives un-prompted; the package managers
  above are allowed to do it because that is their job and their lockfiles
  are reviewable.
- **`env` and `cat`.** The worker container holds `GH_TOKEN` and
  `CLAUDE_CODE_OAUTH_TOKEN`, so `env` — or `cat /proc/self/environ` — dumps
  both into a transcript the dashboard renders. Neither is needed: `Read` is
  free and unprompted, and covers every legitimate use of `cat`.

An allow-rule here is not a small convenience — it removes `canUseTool` from
the path entirely for the commands it matches, which is the same authority
`allowedTools` carries in `worker/src/session.ts`. That file deliberately
keeps `Write`/`Edit`/`Bash` out of `allowedTools`; a rule added here is the
one other way to undo that, so add one the way you would add an entry there.

## The `ask` counterweight lives in `worker/src/session.ts`, not here

Since docs/adr/0049 a session also loads the **target repo's** own
`.claude/settings.json` (`settingSources: ["user", "project"]`), which is how
it gets that repo's `CLAUDE.md` and `.claude/skills/`. That merges the repo's
`permissions.allow` into the same set as this file's — so without a
counterweight, any onboarded repo could ship `Bash(gh api:*)` in its own
settings and approve its own outward-facing commands.

That counterweight is `FLEET_ASK_RULES` in `worker/src/session.ts`, passed to
`query()` through `settings` (scope `flagSettings`, always collected). It is
the same list, doing the same job — `git push`, `gh`, `rm`, `sudo`, `kubectl`,
`curl`, `wget`, `env` — and it is deliberately **not** a `permissions.ask`
block in this file, because since SDK 0.3.233 the evaluator returns on an
ask-rule match *before* the permission mode's own allow short-circuit. Shipped
as a file it would apply to `bypassPermissions` too, leaving a session a human
explicitly gave total authority unable to `git push` (docs/adr/0052).

`ask` rather than `deny` because a human should still be able to approve a
`git push` — every result is a PR, and approving it *is* the review.

**There is no specificity tiebreak, so `ask` beats a narrower `allow` too.**
`Bash(curl http://127.0.0.1:*)` and `Bash(curl http://localhost:*)` used to be
on the allow list; `Bash(curl:*)` in the ask list matches those commands first,
which would have made the two allow entries dead text claiming a permission
they no longer granted. They were removed instead. Localhost `curl` prompts
like any other — cheap, since builds and tests are what actually run in a loop,
and `curl` is the one entry whose un-prompted form is a supply-chain path.

A pair like that is the thing to check when adding to either list: a broad
`ask` prefix silently swallows every narrower `allow` beneath it.

**Hooks do not belong in this `settings.json`.** The Agent SDK does not run
hooks declared in settings files, with any `settingSources` — verified
2026-08-13, after the `rtk hook claude` PreToolUse entry that lived here
turned out to have never fired once. Only `options.hooks` callbacks run, so a
worker hook was registered in `worker/src/session.ts` instead. That hook is
now gone too: it was scoped to the sandbox's `run_command`, which no longer
exists, and it could not be repointed at `Bash` — the SDK discards a hook's
`updatedInput` unless the hook also returns `permissionDecision: "allow"`,
which for `Bash` would bypass the human gate. `rtk` now runs from
`canUseTool` only, so it compacts prompted commands and not allow-listed ones
(see `worker/src/rtkHook.ts`). Everything else here — `CLAUDE.md`, `skills/`,
`enabledPlugins` — is discovered natively as described above.

`ponytail`/`caveman` marketplace plugins are baked into `worker/Dockerfile`
at build time, then copied once (guarded, no runtime `plugin add`) from the
image's baked-in location into `$CLAUDE_CONFIG_DIR/plugins` by
`worker/entrypoint.sh` on first pod boot — see docs/adr/0033. This file's
`enabledPlugins`/`extraKnownMarketplaces` in `settings.json` is what
actually turns them on, since `SyncFleetShared` overwrites
`$CLAUDE_CONFIG_DIR/settings.json` from here every dispatch.
