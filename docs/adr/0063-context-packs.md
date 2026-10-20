# ADR-0063 — Context packs: one surface for the rules and skills fleet agents load

- **Status:** Proposed
- **Date:** 2026-10-19
- **Related:** replaces the plan's "P2 Genome" (a SHA-pinned slice of the
  private claude-config repo; this ADR keeps the read-only delivery and drops
  the source). [ADR-0062](0062-defined-agents.md) §2 (packs that need review are
  read at the release tag, like definitions) and §7 (`systemPrompt` and `skills`
  stay inert; this does not wire them). [ADR-0064](0064-definition-pod-profile.md)
  (`RemoveFleetShared` is untouched; the genome is a sibling of claude-home,
and only Decision 8 changes what `SyncFleetShared` deletes there).
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) (egress stays the
  vault's decision; a pack cannot widen it). [ADR-0049](0049-project-setting-source.md)
  (the `project` setting source stays, see Decision 6). [ADR-0048](0048-one-session-one-pod-one-shared-home.md)
  (prompt snippets were human-visible composer prefill, kept separate from
  packs; they were later removed, migration 000013). The trailer enforcement this ADR relies on shipped in #269 (4.24.0).
  [ADR-0074](0074-fleet-repos-come-from-github.md) (a repo attachment
  names a `repos` row, which now follows GitHub). The collaborator editor
  role gets its own later ADR.

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

Pack names match `skillRe`. Live and reviewed packs share one namespace: a
live pack cannot take a name that exists under `packs/`, and a reviewed pack
that arrives with a live pack's name fails the definitions sync until one is
renamed.

- **Live packs** live in core's Postgres. Tables: `packs(name, owner_group)`,
  `pack_versions(pack, version, text, skills, has_mcp, created_by, created_at)`,
  and `pack_attachments(target_kind ∈ {definition, repo}, target, pack, attached_by)`.
  They are edited in the console and take effect on the next warm.
  - A repo attachment is keyed by `repos.github_id` (ADR-0074), with no
    foreign key. An override rename does not touch it. When the 7-day sweep
    deletes a repo, the attachment stays as an orphan, and it applies again if
    the same GitHub repo rejoins. `sessions.repo` keeps the name it was opened
    with, so render resolves the id through that name; a session opened before
    an override rename renders without the repo's packs, and its log entry
    says so.
  - A definition attachment names a definition that lives in git. When a
    definitions sync no longer lists that name, core deletes its attachments,
    so a later definition with the same name, possibly another audience,
    starts with none.
- **Reviewed packs** live in this repo under `packs/<name>/`, next to
  `definitions/`. The provisioner reads them at the release tag together with
  the definitions, and core keeps the last good copy in a `reviewed_packs`
  table (the `agent_definitions` pattern).
  - `packs/fleet` applies to every pod.
  - `packs/repo` applies to every repo session by kind, with no attachment row,
    so a newly onboarded repo is covered.
  - Any pack that carries MCP servers or plugins must be a reviewed pack.
  - Layout: `packs/<name>/PACK.md` (the text) and
    `packs/<name>/skills/<skill>/SKILL.md`. MCP and plugin files are defined in
    the later phase that wires them. The provisioner's definitions read is
    extended to `packs/`; today it reads `definitions/` only.
  - Reviewed packs are owned by `platform-admins`.

Core never writes to a git repository, and claude-config is never written by
the fleet.

### 2. What a live pack may contain

Text and skills only.

- Skill names must match the definitions validator's `skillRe`.
- Skill frontmatter is an allowlist, `name` and `description`. Anything else,
  such as `allowed-tools`, `hooks` or `model`, is refused at save.
- A skill body containing `` !` `` dynamic-context syntax is refused until it is
  shown not to bypass `canUseTool`.
- Text and skill bodies may not contain `@path` memory imports. An import
  would pull in files the pack does not hold, such as `/proc/self/environ` (a
  repo pod's real tokens) or files the agent can write under `/workspace`.
  Refused at save and again at render.
- A live skill may not reuse a skill name from a reviewed pack. Refused at
  render, since a reviewed pack can gain the name later.
- Caps, checked at save, at attach and at render, because composed sizes are
  sums: 64 KiB of composed memory text per target, which loads on every turn,
  and 256 KiB of composed skill bodies, which load only when a skill is used.

`has_mcp` is decided at **render**: a live-attached pack that carries MCP is
skipped and logged. An attach-time check alone would miss a reviewed pack that
gains MCP in a later release. Reviewed MCP packs reach an agent only through
its definition YAML, in a later phase.

### 3. Ownership

A pack has an `owner_group`. An editor may change a pack only if every target
it is attached to is in their scope. A pack owned by a group cannot be
attached outside that group. Without this rule, editing a pack shared by
`opus-workspace` and `dev-notes` would change an agent the editor has no right
to. A repo has no group (`repos` has no owner column), so repo targets are
`platform-admins` scope only until the editor ADR gives repos one. The editor
role itself is a later ADR. Until then the console is `platform-admins` only.

A live pack attached to a definition changes what that pinned, reviewed
definition loads, without review and without the revocation ADR-0062 §3 applies
to a sha change. That is the point of live packs and is accepted: packs carry
text and skills only (Decision 2), never tools, permissions or MCP, and every
warm logs the versions it rendered (Decision 7).

### 4. Every warm loads the latest state

Freshness wins over a per-session pin.

- `WarmIfIdle` is the only path to a worker pod. It calls one core function,
  `RenderContext(target)`, which makes one indexed read and produces the bytes.
- Bytes are rendered into one file per layer: `CLAUDE.md` and
  `.claude/rules/NN-<pack>.md`, plus `.claude/skills/<name>/SKILL.md`. They are
  sent in `CreateWorkerPodRequest`.
- A resumed conversation can therefore load changed rules. That is the
  accepted cost, and Decision 7 makes it visible.

**Invariant: every warm goes through `WarmIfIdle`.** That includes the warm
`AgentService.Open` performs (P4; it does not exist yet), which is how
ADR-0071's dispatcher (proposed in #257) reaches a definition pod. Bridge sessions then get packs with nothing
extra. The ADR-0072 executor runs approved steps in its own pod and never
warms a session pod, so it renders nothing. A new pod path that skips
`WarmIfIdle` must call `RenderContext` itself.

### 5. Delivery: a read-only directory per session

- The provisioner writes the bytes into the session's own
  `claude-home/<id>-genome/`, rewritten on every warm.
  - It writes only `CLAUDE.md`, `.claude/rules/NN-<pack>.md` and
    `.claude/skills/<skill>/SKILL.md`, with `<pack>` and `<skill>` matching
    `skillRe`. Any other path is refused, so no `.claude/settings.json` or
    `.mcp.json` can appear.
  - Files are opened with `O_NOFOLLOW`, and only regular files are written.
  - It writes `<id>-genome.new`, renames the old directory aside, renames the
    new one into place, and removes the old one. The pod mounts the
    directory when it is created, after the swap, so a running pod never sees
    a half-written genome.
- That directory is mounted **read-only** at `/genome`. The worker passes
  `additionalDirectories: ["/genome"]` and
  `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`.
- `DeleteSessionDir` (the session sweep's provisioner half) removes
  `<id>-genome` next to `claude-home/<id>`, so there are no shared hash-named
  directories and no garbage collector racing a pending pod. It is a sibling,
  not a child, of `claude-home/<id>`: a child would also be reachable through
  the read-write `/claude-home` mount.
- The response carries `genome_applied`. A provisioner replica from before this
  change drops the field, the same overlap `DefinitionProfileApplied` guards.
  For a definition pod core refuses the warm, as it does for the profile. For
  a repo pod it logs the mismatch. `genome_applied` only proves the files were
  written; whether the worker loaded them is Decision 7's report, which also
  catches a repo pinned to an older worker image.
- If `reviewed_packs` is stale because the definitions sync is down, the warm
  uses the last good copy of the text-only packs (`packs/fleet`, `packs/repo`)
  and marks the log entry `stale`; a missing fleet text would be worse than an
  old one. A reviewed pack that carries MCP or plugins follows the definitions
  rule instead and fails closed past `staleAfter` (ADR-0062), so a narrowing
  is never covered by the wider old copy.

### 6. Load order is the SDK's, and the space loses ties

Spiked against SDK 0.3.303 with an empty user scope. The worker step reruns it
with the real user scope (fleet-shared and the image plugins) as a golden test
on `memoryFiles` and the skills list:

- With the env set, `/genome/CLAUDE.md` and each `/genome/.claude/rules/*.md`
  load as separate memory files, typed `Project`. Without the env, none load.
- Skills under `/genome/.claude/skills/` load either way.
- Memory files from `additionalDirectories` load in array order, **after**
  the working directory's own project files.
- With `settingSources: ["user"]` the order could be made fleet → packs →
  workspace, but then **no skills load at all**, neither genome nor workspace
  ones. Six of the nine onboarded repos ship `.claude/skills`.

So `settingSources` stays `["user", "project"]`. The workspace's
`.claude/CLAUDE.md` loads first. Inside `/genome` the render numbers the rules
files so that live packs come before reviewed ones: live packs as
`20-<pack>.md`, then `packs/repo` as `80-repo.md` (on a repo session), then
`packs/fleet` as `90-fleet.md`, with `CLAUDE.md` left empty. Later memory has
precedence, so on a conflict reviewed text beats an unreviewed live pack, and
any pack beats the space's own file. The golden test pins this order.
Precedence must never be the only thing between an agent and a rule that
matters: such a rule goes into worker flag scope or a hook, as attribution did
in #269.

### 7. The panel shows what loaded, not what was sent

Each warm leaves two entries in the session's own transcript, both SYSTEM
signals the agent never receives (only human and session entries stream to
it):

- `context_rendered`, from core: the pack versions it sent, what it skipped
  and why (caps, shadowed skills, a stale or failed reviewed copy), and
  whether the provisioner applied them.
- `context_loaded`, from the worker after init: `getContextUsage()`'s memory
  files in load order, every skill with its source, and any name listed
  twice. Collisions come from what the worker actually loaded, because core
  cannot see a workspace's `.claude/skills` or the image's plugins.

The session feed shows both per warm. The worker's entry goes through the
sidecar's existing `/message` path, so it needs no new RPC or auth rule. It
comes from a pod the agent runs in, so it is a diagnostic, not a control:
nothing is allowed or refused on its word. (First drafted as a separate
`pod_context_log` table; the transcript already holds per-warm, per-session
history with the session's lifetime, so a second store bought nothing.)

### 8. fleet-shared moves in two releases

`fleet-shared/` syncs from `main` on every warm. Reviewed packs arrive at the
release tag. Deleting the old file in the same change as adding the pack would
leave pods with no fleet rules between merge and release.

- **Release N:** adds `packs/fleet` and `packs/repo`, deduplicated against
  `fleet-shared/CLAUDE.md`. It also makes `SyncFleetShared` delete `CLAUDE.md` or
  `skills/` when they are missing from the source. Today it skips a missing name,
  which would leave every resumed repo session with a stale copy.
- **Release N+1**, once the `context_loaded` entries show both packs loaded: deletes
  `fleet-shared/CLAUDE.md` and `skills/`, and release N's sync removes the
  synced copies. Rolling back to a release before N afterwards leaves repo
  pods without fleet rules, so a rollback past N restores the files first. `fleet-shared/settings.json` stays; it
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
  hidden injected prompt that ADR-0048 removed. (Snippets were then removed
  outright, not folded: with packs, the console had two places for reusable
  text and the user kept one.)

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
- Every warm adds two transcript entries, kept and swept with the session.

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
