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
an empty name or identity, or with tool keys, is `InvalidArgument`.

`GetVersionResponse.definition_pod_profile = true` says a provisioner applies
it. Core asks on every definition warm (uncached: a cache would need
invalidation on exactly the rollout that changes the answer) and refuses with
`FailedPrecondition` before `ReserveSlot` when the answer is no or the call
fails. That is the check ADR-0062 §4 said this slice would put in place of its
refusal: an older provisioner would ignore the field and build the ordinary pod.

### 2. What a definition pod gets

| | Repo session (unchanged) | Definition session |
|---|---|---|
| Job + pod labels | `WorkerLabels` | + `fleet.bnei.dev/definition: <name>` (P3's vault policy selects on it) |
| worker `LEASE_ID` | present | **absent**: the sidecar is the only holder |
| worker `GH_TOKEN` | present | **absent** |
| worker `CLAUDE_CODE_OAUTH_TOKEN` | present | **present, INTERIM** (below) |
| worker git identity | `gh api user` at runtime | `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from `git.identity`, plus `GIT_CONFIG_*` `safe.directory=/workspace` |
| repo cache, all three containers | `/repo-cache` ← subPath `repos` | `/repo-cache/<repo>` ← subPath `repos/<repo>`, read-only |
| clone init container | `clone --shared`, then `fetch origin <base>` from the real remote | local transport only (below) |
| fleet-shared sync into claude-home | yes | **no** |
| worker image | `repos.image` or the default | always the provisioner's default (core sends `""`) |

The cache keeps its absolute path, so `.git/objects/info/alternates`
(`/repo-cache/<repo>/.git/objects`) resolves in every container. Only the one
repo is visible.

**The lease lives in the sidecar only, for every pod.** The worker read
`LEASE_ID` for one thing: the body of the sidecar's `POST /session-id`. The
sidecar already holds the lease it authenticates with, so it now always sends
its own and ignores a body `leaseId` (an older worker that still sends one
keeps working; an unknown JSON key is ignored). The worker no longer reads
`LEASE_ID`. Repo pods keep the env var because the golden pins them.

**Identity needs no worker change.** `defaultConfigureGitAuth` already returns
at its first line when `GH_TOKEN` is unset, so `gh api user` (which would fail
without a token) never runs. Git reads `GIT_AUTHOR_*`/`GIT_COMMITTER_*` from
the environment, and `GIT_CONFIG_*` replaces the `safe.directory` line that
early return skips (the sidecar's existing trick).

**The clone never contacts the remote.**

```sh
git clone --shared --no-checkout /repo-cache/<repo> /workspace
cd /workspace
git fetch --no-tags origin refs/remotes/origin/<base>
git checkout -B <base> FETCH_HEAD
git remote set-url origin <repoURL>
```

The base is the cache's **remote-tracking** ref, not its local branch, which
stays frozen at the cache's first clone. That makes the cache fetch
load-bearing, so a definition pod calls `git.Manager.EnsureRepoFetched`: it
always fetches, waiting for the repo lock no longer than the request's context
(a `TryLock` poll, since the lock is a plain mutex), where
`EnsureRepoCachedForSession` skips the fetch when the lock is busy. A wait past
the deadline is a clean error before any pod exists. The script is tested by
running it against real git fixtures (a remote that advanced and gained a
branch after the cache's clone), not by string assertions.

**No fleet-shared sync.** Without its `settings.json`, every tool call in a
definition pod goes through `canUseTool`. Cost: no fleet `CLAUDE.md`, skills or
synced plugins until P2's genome. The entrypoint still copies the image's baked
plugins (`worker/entrypoint.sh`), so nothing crash-loops; `EnsureClaudeHome`
still runs. That `cp -r` onto RWX claude-home is the dominant cold-start cost
(spike 7) and is left for P2's read-only genome mount.

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

**Workspace binding (ADR-0062 §7).** The image is bound: core sends `""`, so a
worker image built before this change (still reading `LEASE_ID`/`GH_TOKEN`,
older `sidecarClient`) can never run a definition pod. The URL is not snapshot,
on purpose: in P1 it reaches nothing a definition pod can use. The cache's
content comes from the `origin` it was first cloned from (`ensureLocked` only
ever runs `fetch origin`, it never re-points it), and a repointed `repos.url`
only changes the pod's push remote, for which the pod holds no credential.
`repos` stays admin-only config. **P3 must bind it** before the pod gets a push
credential: the vault's per-definition `credentials.services` paths are what
make a repointed URL unpushable.

### 3. Schema: a lease only a profile-aware core mints

000008's `sessions_definition_no_lease` (no definition session may hold a
lease) is replaced by migration 000009's `sessions_definition_profile_lease`:
a definition session's `lease_id` must equal its `profile_lease_id`, which only
this core's `ReserveSlot` writes, from the same `gen_random_uuid()`. Any older
core (an image rollback with the schema left at 000009, a RollingUpdate
overlap) mints a lease without it, the CHECK refuses the write, and its
`ReserveSlot` fails for a definition session: the guarantee ADR-0062 got from
the schema survives this slice. The down migration clears definition sessions'
leases (revoking their pods' credential) and restores the 000008 CHECK.

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

After ADR-0062's checks (pinned == current, not cluster-access) and before
`ReserveSlot`: the provisioner must report `definition_pod_profile`. Then
`CreateWorkerPod` gets the profile built from the current row's
`git.identity`, image `""`, and no tool keys.

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
- Rollback below this ADR: run the down migration (it revokes live definition
  pods' leases). An image-only rollback is covered by §3.
