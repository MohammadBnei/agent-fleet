# ADR-0064 — Definition pod profile

- **Status:** Accepted (P1 part). P3 extends it with the Agent Vault and the
  egress lock (plan §5, P3 row); those are not built here.
- **Date:** 2026-10-19
- **Related:** [ADR-0062](0062-defined-agents.md) (defined agents; this replaces
  its unconditional warm refusal), [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md)
  (amended: hub-and-spoke made literal for definition pods),
  [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §4 (amended: the
  repo-cache mount), [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md)
  (a second, per-session table on top of its per-method one),
  [ADR-0060](0060-claude-codes-own-cross-session-channel-is-open.md) (amended: closed for definition pods),
  [ADR-0006](0006-git-identity-from-bot-account.md) (amended for definition pods: identity from
  the reviewed file, not `gh api user`), [ADR-0069](0069-component-auth-and-network-fences.md).

## Context

ADR-0062 made definition sessions creatable and refused every warm, because the
only pod the fleet could build was the ordinary one: `GH_TOKEN` and `LEASE_ID`
in the worker container, the whole `/repo-cache` mounted, the fleet-shared
`settings.json` (which pre-approves `Bash(go run:*)`, `Bash(bun x:*)`,
`Bash(npm install:*)` and more: arbitrary code that never reaches `canUseTool`),
and a `CoreService` lease that can call every method: expose a URL, prompt a
peer, read the fleet-wide journal and Loki.

A definition only narrows. Its pod has to narrow too.

## Decision

### 1. The profile is carried by the request, and its absence changes nothing

`CreateWorkerPodRequest.definition` (`DefinitionPodProfile{name,
git_author_name, git_author_email}`) is set by core for a definition session
only, from the pinned row (which `WarmIfIdle` has just checked is the current
one). Its presence switches the pod to the definition profile. Absent, the Job
is **byte-identical** to before: `TestCreateWorkerPod_RepoSessionGolden` pins
the whole manifest, generated from the code before this change. A profile with
an empty name or identity, with tool keys, or with an image override, is
`InvalidArgument`.

An older provisioner ignores the field and builds the ordinary pod, so core
checks **the create's own answer**, the only one that comes from the replica
that built the pod (the provisioner Deployment rolls with the default
RollingUpdate, so in a rollout or rollback two RPCs can reach different
replicas). `CreateWorkerPodResponse.definition_profile_applied` is set only by a
provisioner that applied the profile. A definition warm whose answer lacks it
got the ordinary pod: core clears the lease first (the pod's credential is dead
before it can use it), tears the pod down, records `CRASHED` with the reason,
and returns `FailedPrecondition`. If the teardown fails, core leaves the phase
live instead of writing `CRASHED`, so the startup-stall sweep retries it (the
pod can report no activity without a lease). A later warm retries. This is the
check ADR-0062 §4 said this slice would put in place of its refusal.

No `GetVersion` capability pre-check. It was built and removed in review: it
cannot see which replica serves the create, so it could never replace the check
above, and all it saved was one short-lived ordinary pod on a fully-old
provisioner, whose lease is revoked before it can stream a message.

The older replica also ran its fleet-shared sync into the session's claude-home
before the pod existed, and claude-home is kept across teardowns (it is resume
state). So a definition create removes `fleetSharedNames` (`settings.json`,
`CLAUDE.md`, `skills/`, `plugins/`; the worker entrypoint re-seeds the image's
plugins) from claude-home before building the pod, and fails rather than build
one if it cannot. `projects/` is left alone.

### 2. What a definition pod gets

| | Repo session (unchanged) | Definition session |
|---|---|---|
| Job + pod labels | `WorkerLabels` | + `fleet.bnei.dev/definition: <name>` (P3's vault policy selects on it) |
| worker `LEASE_ID` | present | **absent**: only the sidecar is given it (below) |
| worker `GH_TOKEN` | present | **absent** |
| worker `CLAUDE_CODE_OAUTH_TOKEN` | present | **present, INTERIM** (below) |
| worker `FLEET_DEFINITION` | absent | `<name>`: the worker closes Claude Code's cross-session channel (below) |
| worker git identity | `gh api user` at runtime | `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from `git.identity`, plus `GIT_CONFIG_*` `safe.directory=/workspace` |
| repo cache, all three containers | `/repo-cache` ← subPath `repos` | `/repo-cache/<repo>` ← subPath `repos/<repo>`, read-only |
| clone init container | `clone --shared`, then `fetch origin <base>` from the real remote | local transport only (below) |
| fleet-shared sync into claude-home | yes | **no**, and any copy is removed (§1) |
| worker image | `repos.image` or the default | always the provisioner's default (core sends `""`, the provisioner refuses an override) |

The cache keeps its absolute path, so `.git/objects/info/alternates`
(`/repo-cache/<repo>/.git/objects`) resolves in every container. Only the one
repo is visible.

**The worker no longer needs the lease, for every pod.** The worker read
`LEASE_ID` for one thing: the body of the sidecar's `POST /session-id`. The
sidecar already holds the lease it authenticates with, so it now always sends
its own and ignores a body `leaseId`. The worker still sends `LEASE_ID` (or
`""`) for one release: a worker image newer than its sidecar (a `repos.image`
floating on `latest` before the provisioner rolls `SIDECAR_IMAGE`) talks to a
sidecar that still forwards the body's, and without it resume identity is
silently lost. This is a deliberate one-release deviation from the P1 plan,
which said to drop the field from the worker now and called "new worker, old
sidecar" unreachable: `repos.image` overrides the worker only, so it is
reachable. Repo pods keep the env var because the golden pins them.

The sidecar is the only container **given** the lease, which is not the same as
the only one that can reach it: its telemetry loop runs `git diff` in the
worker-writable `/workspace` with `safe.directory=*`, so repo-local git config
(`core.fsmonitor`, `diff.external`, a clean filter) can run code in the sidecar
and read `LEASE_ID`. The bound on a recovered lease is core's per-lease table
(§4), not the container split: the raw lease reaches exactly what the sidecar's
does. Before P4 leans on the sidecar's own tool shaping as a bound, its git must
stop honoring repo config (or read a `GIT_DIR` it owns).

**Identity needs no worker change.** `defaultConfigureGitAuth` already returns
at its first line when `GH_TOKEN` is unset, so `gh api user` (which would fail
without a token) never runs. Git reads `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from
the environment, and `GIT_CONFIG_*` replaces the `safe.directory` line that
early return skips (the sidecar's existing trick).

**The clone never contacts the remote.**

```sh
git clone --shared --no-checkout /repo-cache/<repo> /workspace
cd /workspace
git fetch --no-tags /repo-cache/<repo> '+refs/remotes/origin/*:refs/remotes/origin/*'
git checkout -B <base> origin/<base>
git remote set-url origin <repoURL>
```

The fetch names the cache's path, not `origin`, so no reordering of these lines
can turn it into a remote fetch. It copies the cache's **remote-tracking** refs
over the workspace's `origin/*`, which `clone --shared` made from the cache's
local branches (frozen at its first clone): the base and every `origin/*` the
agent diffs against are current, with no phantom "ahead N". That makes the cache fetch
load-bearing, so a definition pod calls `git.Manager.EnsureRepoFetched`: it
always fetches, waiting for the repo lock no longer than the request's context
(a `TryLock` poll, since the lock is a plain mutex), where
`EnsureRepoCachedForSession` skips the fetch when the lock is busy. A wait past
the deadline is a clean error before any pod exists. The script is tested by
running it against real git fixtures (a remote that advanced and gained a
branch after the cache's clone), not by string assertions.

**No fleet-shared sync.** Its `settings.json` pre-approves arbitrary-exec
`Bash` fleet-wide, and those pre-approvals are gone from definition pods. That
is all: P1 definition pods still run the worker's default tool surface. The
worker's `allowedTools` (`Read`, `Glob`, `Grep`, `WebSearch`, `WebFetch`,
`Task`, `mcp__agent-fleet-sidecar__*`, `mcp__playwright__*`) skip `canUseTool`,
and `settingSources: ["user", "project"]` still merges the target repo's own
`.claude/settings.json` allow rules (reviewed config in a `repos` repo, the same
trust as a repo session). The definition's `tools.allow`/`deny` are not applied
in P1; only `mode` and `model` are bounded. P4's `AgentSpec` plumbing
(`settingSources: ["user"]` plus `additionalDirectories` for the repo
`CLAUDE.md`, plan §5c spike 4, and `tools.allow` decided inside `canUseTool`)
makes the definition's tool surface the pod's. Cost: no fleet `CLAUDE.md`,
skills or synced plugins until P2's genome. The entrypoint still copies the image's baked
plugins (`worker/entrypoint.sh`), so nothing crash-loops; `EnsureClaudeHome`
still runs. That `cp -r` onto RWX claude-home is the dominant cold-start cost
(spike 7) and is left for P2's read-only genome mount.

**Claude Code's own cross-session channel is closed (amends ADR-0060).** Every
pod passes `crossSessionInbound: "accept"` and lists `SendMessage`/`ListAgents`
in `allowedTools`, and every pod shares one OAuth account. That channel reaches
every session on the account (other pods, laptop sessions) around core, the
transcript and the lease, so §4's "no peers, not promptable" would bound nothing
there. `FLEET_DEFINITION` (set only by this profile) makes the worker pass
`crossSessionInbound: "refuse"`, drop `SendMessage`/`ListAgents` from
`allowedTools` and add them to `disallowedTools`. It is a flag, not P4's
`AgentSpec`. It needs this release's worker, which is why the image is bound
(below).

**INTERIM: `CLAUDE_CODE_OAUTH_TOKEN` stays in definition pods in P1.** The
model credential moves to the Agent Vault in P3. In P1 only platform-admins
launch definition sessions, so the trust level equals today's; the assertion
"no real secret in the pod" becomes true in P3, and the pod-shape test asserts
the token's presence on purpose, so P3 has to flip it consciously. No
placeholder is invented now.

**P1 definition sessions cannot push or open a PR.** No `GH_TOKEN` and no
credential for the real `origin`. They read, edit and commit locally, as
verification vehicles until P3's vault. "Every result = PR" is not met by them
in P1; the kind QA asserts that `git push` fails.

**Workspace binding (ADR-0062 §7), partly.** The image is bound to the
release, not snapshot per session: core sends `""` and the provisioner refuses
an override for a definition pod, so it always runs the provisioner's own
default worker. The reason is the worker-side hardening above:
`FLEET_DEFINITION` does nothing in a worker built before it, and a `repos.image`
override (a repo toolchain image, possibly floating or pinned old) is not
guaranteed to be this release's worker. Cost: a definition on a repo with its
own toolchain image runs on the default image. ADR-0062 §7's literal ask (snapshot
at create, refuse a warm when it differs) is not done: the default image moves
with every provisioner rollout, so a resumed definition session may run a newer
worker than it was created on.

The URL is not snapshot either. It does decide content on a **cold** cache:
`ensureLocked` runs `git clone <repos.url>` when the cache does not exist, and
only `fetch origin` after (it never re-points a warm cache). So a repointed
`repos.url` decides the code a definition session runs when that repo's cache is
first created, and changes the pod's push remote, for which the pod holds no
credential in P1. `repos` is admin-only config, as `repos.url` already decides
the code of every repo session. **P3 must bind it** before the pod gets a push
credential: the vault's per-definition `credentials.services` paths are what
make a repointed URL unpushable.

### 3. Schema: a lease no older core can mint or verify

An older core knows nothing of definitions. It would warm a definition session
as a repo session (`GH_TOKEN` and `LEASE_ID` in the worker, the whole cache, any
mode), and it authenticates a lease against `lease_id` with no definition
allowlist, no `from` check and no `mode.allow` bound. Both have to be closed in
the schema, because an image rollback with the schema left at 000009, or a
RollingUpdate overlap (the migration runs PreSync), puts one in front of a live
definition pod.

So a definition session's lease never lives in `lease_id`. 000008's
`sessions_definition_no_lease` CHECK stays: no definition session holds a
`lease_id`, so an older core's `ReserveSlot` fails for one. Migration 000009
adds `profile_lease_id`, which only this core's `ReserveSlot` writes, for a
definition session only. This core's `VerifyLease`, `SaveAgentSessionID` and
`Get` read `COALESCE(lease_id, profile_lease_id)`; `ClearLease` (teardown and
the reconcile revocation) clears both. An older core matches only `lease_id`,
so a definition pod's lease authenticates nothing there: it fails closed, it is
not honored without the allowlist. The down migration drops the column, which
revokes live definition pods' leases. A second CHECK,
`sessions_profile_lease_definition_only`, is the other direction: a repo
session never holds a `profile_lease_id`, which would verify with no allowlist.

An older core cannot **revoke** a definition lease either: its `ClearLease`
nulls `lease_id` only. So `VerifyLease` and `SaveAgentSessionID` accept
`profile_lease_id` only while `pod_phase` is live. Whichever core tears the pod
down, the provisioner reports `TERMINATED` (or reconcile writes it for a lost
event), and that revokes it. A repo lease keeps ADR-0057's rule (valid until
cleared or rotated).

### 4. CoreService: a per-session allowlist for definition leases

`LeaseVerifier.VerifyLease` returns the session's `definition_name` in the same
query. After `authorize` (the ADR-0057 binding and the fail-closed
unknown-method rule still run first), a definition lease may call only
`definitionMethods` (`core/internal/coreserver/auth.go`); anything else is
`PermissionDenied`. Provisioner-token callers never reach the table. It is a
small fixed table, derivable from `sidecarTools` in P4 (tool → method), not a
framework. A test iterates `CoreService_ServiceDesc` so a new RPC must be put in
one column or the other.

| Allowed | Why |
|---|---|
| `SendMessage`, `WaitForMessages`, `StreamHumanMessages` | the transcript, both directions |
| `AskUserQuestion` | `sidecarTools: AskUserQuestion` |
| `GetSession` | the worker's startup mode/model restore |
| `SetPermissionMode` | the worker persists its mode; `sessions.Store.SetPermissionMode` bounds it by `mode.allow` (ADR-0062), `PermissionDenied` outside |
| `UpdateSessionMeta` | `set_session_meta` |
| `SaveAgentSessionId` | resume identity (its model is ignored for definitions, ADR-0062) |
| `PushToolTelemetry` | the sidecar's telemetry loop |

| Denied | Why |
|---|---|
| `Expose`, `Unexpose` | publishes a URL: widens the network surface |
| `RequestService` | provisions cluster Postgres/Redis |
| `AppendJournal`, `SearchJournal` | the fleet-wide journal other sessions read: a poisoning vector and a fleet-wide read |
| `ListFiles`, `GetFileUploadUrl`, `GetFileDownloadUrl`, `DeleteFile` | fleet-wide shared files |
| `ViewLogs` | fleet-wide Loki |
| `ListPeerSessions`, `PromptSession`, `WaitForSessionState` | peer discovery and peer prompting; agent→agent goes through the event bridge (`publish_event` → `Open`, P4) |

**PromptSession into a definition session is refused** in the handler, from any
caller (provisioner token included), right after the target is read:
definition sessions are not promptable, use the event bridge.

**`SendMessage`: every lease caller authors as `agent` or `worker`.** This is a
fleet-wide fix, not a definition rule. Core streams `human` and `session`
entries to the worker, and the worker obeys any delivered `permission_response`
and `permission_mode` regardless of who wrote it. The sidecar's localhost
`POST /message` forwards an arbitrary `from`, and any process in the worker
container can reach it, so a prompt-injected agent in **any** session could
approve its own pending prompt or switch itself to `auto`. Every pod writer uses
`agent` (the worker's relays, system notes, the sidecar's MCP tools) or
`worker` (`session_failed`); `human` is written only by DashboardService and
`session` only by core's own `PromptSession`, neither through a lease. So
`authorize` refuses `SendMessage` from a lease with any other `from`
(`PermissionDenied`). The MCP `send_message` tool loses its `from` parameter
(it advertised `'agent' | 'human'`) and stamps `agent`. ADR-0062's narrower
handler check (definition sessions only, `human`/`session` only) is subsumed
and removed.

### 5. Core's warm, for a definition session

After ADR-0062's checks (pinned == current, not cluster-access) and
`ReserveSlot`, `CreateWorkerPod` gets the profile built from the current row's
`git.identity`, image `""`, and no tool keys, and its answer must carry
`definition_profile_applied` (§1).

## Consequences

- Amends ADR-0020: the worker container of a definition pod holds no
  credential to core at all, only the sidecar does. Amends ADR-0048 §4: the
  read-only cache mount is per repo for definition pods. Amends the locked
  decision "git commit identity derived live from `gh api user`, never
  hardcoded" and the matching forbidden pattern: for definition pods the
  identity is declared in the reviewed definition file, not in code.
- **Recorded asymmetry, a P4 item:** repo sessions can still see definition
  sessions through `ListPeerSessions`/`WaitForSessionState` (and Loki through
  `ViewLogs`), while definitions cannot see them. Harmless while only
  platform-admins launch definitions; P4 filters before audiences exist.
- The `SendMessage` from-check changes behavior for every session; only writes
  no legitimate pod caller makes are refused.
- Egress is still open in P1; the lock is P3 (plan §5).
- Amends ADR-0060 for definition pods: the native cross-session channel is
  refused inbound and its tools are disallowed (§2).
- Rollback below this ADR: run the down migration (it revokes live definition
  pods' leases). An image-only core rollback is covered by §3: the older core
  neither mints nor verifies a definition lease, so live definition pods lose
  CoreService instead of getting an unbounded one, and the pod phase revokes
  what its `ClearLease` misses. A provisioner rollback is covered by §1's
  check on the create's answer and its claude-home scrub.
- **P1 does not bound the tool surface** (§2, "No fleet-shared sync"): the
  worker's `allowedTools` and the target repo's project allow rules still skip
  `canUseTool`. P4.
