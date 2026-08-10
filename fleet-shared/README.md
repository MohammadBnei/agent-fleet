# fleet-shared

The git-tracked source for every worker pod's shared Claude Code context —
implements docs/adr/0019 point 6 / docs/adr/0032.

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
bundle beyond a plain skill — see docs/adr/0032's "Out of scope"), and
marketplace-installed plugins like `ponytail`/`caveman` (deferred — they'd
need their own install/refresh mechanism, not a per-dispatch marketplace
clone).
