# ADR-0063 — Context packs: one surface for the rules and skills fleet agents load

- **Status:** Proposed
- **Date:** 2026-10-19
- **Related:** replaces the plan's "P2 Genome" (a SHA-pinned slice of the
  private claude-config repo; this ADR keeps the read-only delivery and drops
  the source). [ADR-0062](0062-defined-agents.md) §2 (packs that need review are
  read at the release tag, like definitions) and §7 (`systemPrompt` and `skills`
  stay inert; this does not wire them). [ADR-0064](0064-definition-pod-profile.md)
  (`RemoveFleetShared` is untouched: nothing here writes into claude-home).
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) (egress stays the
  vault's decision; a pack cannot widen it). [ADR-0049](0049-project-setting-source.md)
  (the `project` setting source stays, see Decision 6). [ADR-0048](0048-one-session-one-pod-one-shared-home.md)
  (prompt snippets are human-visible composer prefill and stay separate from
  packs). The trailer enforcement this ADR relies on shipped in #269 (4.24.0).
  The collaborator editor role is ADR-0074.

## Context

The rules an agent works under live in three copies that drift:

- the laptop config repo (`MohammadBnei/claude-config`);
- `fleet-shared/` here, synced into repo sessions' claude-home from `main` on
  every warm;
- each workspace's own `.claude/CLAUDE.md`.

Definition pods get none of `fleet-shared/` (ADR-0064), so a definition
agent's only rules are its workspace's.

On 2026-10-19 a `dev-notes` agent added `Co-Authored-By: Claude Opus 5` to a
commit although the notes space's `CLAUDE.md` forbade it. Claude Code's own
attribution instruction beat the text. Text cannot hold a rule that matters,
and #269 moved that one into config and a git hook.

What is wanted: reusable **packs** of rules text and skills (later MCP and
plugin references) that compose onto agents and repos. They must be edited
live from the console, with no release, by the owner and later by one
collaborator for their own agents. Anything that widens authority still goes
through review. And every warm must show what it actually loaded.

## Decision

### 1. Two sources, one model

A **pack** is a name plus immutable versions. Each version is
`{text, skills: [{name, body}], has_mcp}`.

- **Live packs** live in core's Postgres. Tables: `packs(name, owner_group)`,
  `pack_versions(pack, version, text, skills, created_by, created_at)`, and
  `pack_attachments(target_kind ∈ {definition, repo}, target, pack, attached_by)`.
  They are edited in the console and take effect on the next warm.
- **Reviewed packs** live in this repo under `packs/<name>/`, next to
  `definitions/`. The provisioner reads them at the release tag together with
  the definitions, and core keeps the last good copy in a `reviewed_packs`
  table (the `agent_definitions` pattern).
  - `packs/fleet` applies to every pod.
  - `packs/repo` applies to every repo session by kind, with no attachment row,
    so a newly onboarded repo is covered.
  - Any pack that carries MCP servers or plugins must be a reviewed pack.

Core never writes to a git repository, and claude-config is never written by
the fleet.

### 2. What a live pack may contain

Text and skills only.

- Skill names must match the definitions validator's `skillRe`.
- Skill frontmatter is an allowlist, `name` and `description`. Anything else,
  such as `allowed-tools`, `hooks` or `model`, is refused at save.
- A skill body containing `` !` `` dynamic-context syntax is refused until it is
  shown not to bypass `canUseTool`.
- The composed size is capped at 1 MiB. It is checked at save, at attach, and at
  render, because the composed size is a sum.

`has_mcp` is decided at **render**: a live-attached pack that carries MCP is
skipped and logged. An attach-time check alone would miss a reviewed pack that
gains MCP in a later release. Reviewed MCP packs reach an agent only through
its definition YAML, in a later phase.

### 3. Ownership

A pack has an `owner_group`. An editor may change a pack only if every target
it is attached to is in their scope. A pack owned by a group cannot be
attached outside that group. Without this rule, editing a pack shared by
`opus-workspace` and `dev-notes` would change an agent the editor has no right
to. The editor role itself is ADR-0074. Until then the console is
`platform-admins` only.

### 4. Every warm loads the latest state

Freshness wins over a per-session pin.

- `WarmIfIdle` is the only path to a worker pod. It calls one core function,
  `RenderContext(target)`, which makes one indexed read and produces the bytes.
- Bytes are rendered into one file per layer: `CLAUDE.md` and
  `.claude/rules/NN-<pack>.md`, plus `.claude/skills/<name>/SKILL.md`. They are
  sent in `CreateWorkerPodRequest`.
- A resumed conversation can therefore load changed rules. That is the
  accepted cost, and Decision 7 makes it visible.

The bridge executor (ADR-0072, proposed in #257) creates pods on its own path. **It must call
`RenderContext`**, or bridge sessions run without packs.

### 5. Delivery: a read-only directory per session

- The provisioner writes the bytes into the session's own
  `claude-home/<id>-genome/`, rewritten on every warm, into a fresh directory.
  - Names are validated.
  - Files are opened with `O_NOFOLLOW`.
  - Only regular files are written.
- That directory is mounted **read-only** at `/genome`. The worker passes
  `additionalDirectories: ["/genome"]` and
  `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`.
- `DeleteSessionDir` (the session sweep's provisioner half) removes
  `<id>-genome` next to `claude-home/<id>`, so there are no shared hash-named
  directories and no garbage collector racing a pending pod. It is a sibling,
  not a child, of `claude-home/<id>`: a child would also be reachable through
  the read-write `/claude-home` mount.
- The response carries `genome_applied`. A provisioner replica from before this
  change drops the field, the same overlap `DefinitionProfileApplied` guards,
  and core logs or refuses the mismatch.
- If `reviewed_packs` is stale because the definitions sync is down, the warm
  uses the last good copy and marks the log entry `stale`. A warm never blocks
  on the sync.

### 6. Load order is the SDK's, and the space loses ties

Spiked against SDK 0.3.303 with an empty user scope:

- With the env set, `/genome/CLAUDE.md` and each `/genome/.claude/rules/*.md`
  load as separate memory files, typed `Project`. Without the env, none load.
- Skills under `/genome/.claude/skills/` load either way.
- Memory files from `additionalDirectories` load in array order, **after**
  the working directory's own project files.
- With `settingSources: ["user"]` the order could be made fleet → packs →
  workspace, but then **no skills load at all**, neither genome nor workspace
  ones. Six of the nine onboarded repos ship `.claude/skills`.

So `settingSources` stays `["user", "project"]`, and the real order is:
workspace `.claude/CLAUDE.md` first, then `packs/fleet`, then `packs/repo` (on a
repo session), then the attached live packs. Later memory has precedence, so
on a conflict a pack or the fleet text wins over the space's own file.
Precedence must never be the only thing between an agent and a rule that
matters: such a rule goes into worker flag scope or a hook, as attribution did
in #269.

### 7. The panel shows what loaded, not what was sent

After init the worker reports `getContextUsage().memoryFiles`, its skill names
and any name collisions through the sidecar. Core stores
`pod_context_log(session, warm_at, image, pack_versions, memory_files, skills, collisions, stale)`.
The session view shows it per warm. Collisions are reported from what the
worker actually loaded, because core cannot see a workspace's
`.claude/skills` or the image's plugins.

### 8. fleet-shared moves in two releases

`fleet-shared/` syncs from `main` on every warm. Reviewed packs arrive at the
release tag. Deleting the old file in the same change as adding the pack would
leave pods with no fleet rules between merge and release.

- **Release N:** adds `packs/fleet` and `packs/repo`, deduplicated against
  `fleet-shared/CLAUDE.md`. It also makes `SyncFleetShared` delete `CLAUDE.md` or
  `skills/` when they are missing from the source. Today it skips a missing name,
  which would leave every resumed repo session with a stale copy.
- **Release N+1**, once `pod_context_log` shows both packs loaded: empties
  `fleet-shared/CLAUDE.md` and `skills/`. `fleet-shared/settings.json` stays; it
  holds repo sessions' permission rules, which are authority and never a pack.

## Alternatives considered

- **Git in claude-config as the pack store**, with core committing live edits
  through a GitHub App. Rejected after a doubt round:
  - That private repo cannot have branch protection on the current plan, so
    "reviewed paths" would be enforced by nothing.
  - A `contents: write` key there also covers the laptops' `settings.base.json`,
    `statusline.sh`, `tools.json` and pre-commit hook. A core bug becomes code
    execution on a workstation at the next `gh ccsync`.
  - Core has no git and no PVC (distroless, zero-RBAC); adding an RWO volume
    forces `Recreate` and console downtime.
  - Every warm would gain a GitHub fetch on the hot path.
- **Postgres with a git export:** two stores that must agree; the mirror drifts.
- **A new fleet-context repo:** a second config source spread across repos, the
  island the feature exists to remove.
- **Pin the rules per session, optionally with a revocation list:** rejected in
  favour of freshness. Rules may change across warms, and Decision 7 shows it.
- **The provisioner fetches packs itself:** a second credential and a second
  copy. Core already holds the data and sends bytes.
- **Fold prompt snippets into packs:** snippets are text a human sees and edits
  before sending. Packs are loaded silently. Merging them would bring back the
  hidden injected prompt that ADR-0048 removed.

## Consequences

- One console page edits what every agent loads. A wrong live pack reaches
  every attached target at its next warm, logged but not reviewed. Ownership
  scoping (Decision 3) bounds who can do that.
- A space's own `.claude/CLAUDE.md` loses conflicts with packs and fleet text
  (Decision 6).
- A space's `.claude/settings.json` still merges hooks, env and allow rules, as
  today. Closing that is P4's switch to `settingSources: ["user"]`, which first
  needs another way to load skills.
- With one human, "reviewed" means an owner-merged PR with green CI. GitHub
  rejects GitHub Actions as a ruleset bypass actor on a personal-account repo, so
  `main` stays unprotected until the release workflow pushes with a deploy key.
  That is tracked, not done.
- The `pod_context_log` table grows by one row per warm. It shares the
  session's retention.

## Rollout

1. This ADR.
2. Core: migrations, store, `RenderContext`, reviewed-pack sync.
3. Provisioner: `packs/` at the release tag, the per-session genome
   directory, `genome_applied`.
4. Worker: `additionalDirectories` + env, the post-init context report.
5. Console: the Packs page and the per-warm context panel.
6. fleet-shared migration, releases N and N+1 (Decision 8).

Each step is its own PR and merged alone; consecutive merges cancel each
other's release run.
