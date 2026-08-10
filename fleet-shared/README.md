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

`ponytail`/`caveman` marketplace plugins are baked into `worker/Dockerfile`
at build time, then copied once (guarded, no runtime `plugin add`) from the
image's baked-in location into `$CLAUDE_CONFIG_DIR/plugins` by
`worker/entrypoint.sh` on first pod boot — see docs/adr/0033. This file's
`enabledPlugins`/`extraKnownMarketplaces` in `settings.json` is what
actually turns them on, since `SyncFleetShared` overwrites
`$CLAUDE_CONFIG_DIR/settings.json` from here every dispatch.
