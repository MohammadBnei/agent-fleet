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

**Hooks do not belong in this `settings.json`.** The Agent SDK does not run
hooks declared in settings files, with any `settingSources` — verified
2026-08-13, after the `rtk hook claude` PreToolUse entry that lived here
turned out to have never fired once. Only `options.hooks` callbacks run, so a
worker hook is registered in `worker/src/session.ts` (see
`worker/src/rtkHook.ts`). Everything else here — `CLAUDE.md`, `skills/`,
`enabledPlugins` — is discovered natively as described above.

`ponytail`/`caveman` marketplace plugins are baked into `worker/Dockerfile`
at build time, then copied once (guarded, no runtime `plugin add`) from the
image's baked-in location into `$CLAUDE_CONFIG_DIR/plugins` by
`worker/entrypoint.sh` on first pod boot — see docs/adr/0033. This file's
`enabledPlugins`/`extraKnownMarketplaces` in `settings.json` is what
actually turns them on, since `SyncFleetShared` overwrites
`$CLAUDE_CONFIG_DIR/settings.json` from here every dispatch.
