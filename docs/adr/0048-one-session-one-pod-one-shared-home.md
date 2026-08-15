# ADR-0048 — One session, one pod, one shared home

- **Status:** Accepted
- **Date:** 2026-08-14
- **Supersedes:**
  - [ADR-0016](0016-task-crash-recovery-and-retry.md) — entirely. The
    lease/heartbeat/retry machine is replaced by reconciling against Kubernetes.
  - [ADR-0023](0023-worktree-reuse-and-branch-sweep.md) — entirely. The fleet no
    longer owns worktrees or branches, so it can no longer destroy them.
  - [ADR-0034](0034-environment-recipe-system.md) and
    [ADR-0036](0036-e2e-recipe-visible-and-override-approved.md) — entirely. The
    agent reads the repo instead of the fleet storing a recipe for it.
  - [ADR-0039](0039-e2e-pod-is-the-worker-sandbox.md) and
    [ADR-0044](0044-e2e-pod-outlives-the-app.md) — entirely. The sandbox merges
    into the worker pod.
  - [ADR-0045](0045-service-endpoint-roster-direct-dial.md) — entirely. With one
    pod there is nothing to dial.
  - [ADR-0012](0012-e2e-provisioner-standalone-app.md) — its pod-provisioning
    half. The per-task subdomain
    ([ADR-0038](0038-per-task-subdomain-e2e-preview.md)) survives.
  - [ADR-0019](0019-shared-pvc-and-unified-provisioner.md) point 1's mount shape
    and the worktree half of point 2. The shared PVC and the single-writer
    provisioner both survive.
- **Amends:**
  - [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md) —
    completes the rename it deferred: *"a future pass could rename the table
    itself if the task/session distinction ever needs to be literal."*
  - [ADR-0032](0032-fleet-shared-pvc-directory.md) — `CLAUDE_CONFIG_DIR` becomes
    per-session rather than one directory shared by every pod.
  - [ADR-0047](0047-metrics-scoped-to-the-hubs.md) — `TasksCurrent{repo,status}`
    has no statuses left to count.
- **Depends on:** nothing outside this repo. An earlier revision required
  `infra-bootstrap` to amend ADR-0002/0026 for an NFS-backed StorageClass;
  §4 was rewritten after measuring, and the fleet stays on `longhorn` plus
  `local-path`, both of which already exist. ADR-0002 stands unamended.

## Context

Two designs in this repo grew the same way, and it is worth naming the shape
before the specifics, because the shape is the finding.

Each began as the fleet doing something on the agent's behalf. Each then
accumulated repair ADRs — not because the repairs were wrong, but because
nobody re-asked whether the thing needed doing at all. `docs/adr/0039`,
`0044` and `0045` are three consecutive documents fixing a sandbox whose
entire purpose is to let **one tool** skip a permission prompt.

### What the tasks model actually is

`tasks` is one row playing three roles: a work item
(`repo`/`description`/`guidance`/`pr_url`), a lease-and-retry record
(`lease_id`/`heartbeat_at`/`retry_count`/`status`), and a session handle
(`session_id`/`pod_phase`/`last_active_at`). ADR-0029 reframed the row as a
session and explicitly declined to split it. The reframing did not hold:

- **`status` has 8 values and `done` has no writer.** Nothing sets it.
  Completion is signalled by `DeriveLiveState`, not by status. `cancelled` is
  overloaded as "ended via Stop" because adding an enum value felt heavier than
  overloading one.
- **The concurrency cap is implemented twice** — `ClaimNextTask` counts
  `status`, `WarmIfIdle` counts `pod_phase` — because ADR-0029 moved lifecycle
  to `pod_phase` and left dispatch behind.
- **`ClaimNextTask` is ~100 lines** of `pg_advisory_xact_lock` +
  `FOR UPDATE SKIP LOCKED` + in-flight counting + retry capping, serving a
  queue that has never had two producers or a backlog.
- **`claimed_by` is never written and never read.** `notes` is written and
  never read. `discord_channel_id` is written and never read.

### What worktree-per-task actually is

`docs/DECISIONS.md` forbids *"a shared writable repo PVC across tasks"*. The
shipped design is one. The comment in `provisioner/internal/k8s/pod.go` says
why, and it is a good reason:

> Whole PVC, not a per-task SubPath: a linked git worktree's `.git` file is an
> absolute-path gitlink back to the main clone's
> `repos/<repo>/.git/worktrees/<taskId>` admin dir — a SubPath scoped to just
> `worktrees/<taskId>` cuts that path off entirely, so every git command in
> this container failed with 'not a git repository'.

So every worker and sidecar mounts the whole volume. Isolation between
concurrent sessions is **directory naming, not a mount boundary** — a session
can read and write every other session's tree, every repo clone, and the
shared `.claude-home`. ADR-0039 relies on the *inverse* of the same mechanism:
the e2e pod's SubPath severs the gitlink, and that severing is its security
control. One mechanism, load-bearing in both directions.

`.claude-home` compounds it. It is **one directory for all five concurrent
pods**, and `SyncFleetShared` runs `rsync -a --delete` over `settings.json`,
`skills/` and `CLAUDE.md` **on every dispatch** — while other sessions are
mid-turn. There is no lock, cross-pod or otherwise. Only `projects/` is safe,
and only because the SDK derives its subdirectory from `cwd`.

### What the sandbox actually buys

Exactly one thing: `run_command` is not `canUseTool`-gated, because the pod it
runs in holds no credentials. ADR-0039 is explicit that keeping `git` out is
what justifies the asymmetry.

That property is thinner than it reads. The agent already has gated `Bash`,
`Write` and `Edit` on the credentialed pod. The moment a human approves one
`bun install` there, the exposure is identical. **The isolation is one click
deep.**

Its cost is not thin: a second pod per session (1 CPU / 1Gi requested, 4 / 4
limit), the `e2e-runner` image, `execmcp`, a per-task NetworkPolicy, the entire
`ServiceEndpoint` roster that ADR-0045 exists to build, a ~65s retry ladder for
when the pod is not up yet, ~24 proxied Playwright tools plus an embedded schema
snapshot that ADR-0044 records as drifting silently, and the SubPath mount whose
gitlink-severing forced the whole-PVC mount that broke isolation in the first
place.

### What the recipe system actually is

`repo_profiles`, `repo_profile_tools` and `repo_profile_services` — three tables
plus `core/internal/e2erecipe` plus `start_cmd` plus `e2e-run-app` and
`e2e-restart-app` — exist to answer "how do I start this app". `cat
package.json` answers it. The fleet is storing, in Postgres, something readable
from the working tree the agent is already sitting in.

### The storage floor

Longhorn RWX is an NFS share-manager pod in front of a 3-replica block volume.
Confirmed live 2026-08-14: one `share-manager` pod on `k8s-worker-01`, serving
every pod on all five nodes, each write fanned to three synchronous replicas.
ADR-0039 measured a cold `bun install` at **782 seconds** and diagnosed it
correctly:

> tens of thousands of tiny file creations, i.e. tens of thousands of NFS round
> trips each fanning out to three synchronous replica writes. That is a
> metadata/IO-bound workload, and no amount of CPU moves it.

And the caches that would amortize it are keyed `cache/<repo>/`, so two repos
pulling the same package cache it twice. A developer's machine does the
opposite: `~/.bun/install/cache` and `~/go/pkg/mod` are global and
content-addressed, shared by every project on the box.

## Decision

### 1. A session is the unit, and the first message boots the pod

```
CreateSession(repo, title?)   -> row only. No pod, no directory.
SendMessage(id, text)         -> if no live pod: provision, THEN append.
StopSession(id)               -> tear down pod; row, tree, SDK state survive
ArchiveSession(id)            -> human is finished; reclaims disk
DeleteSession(id)             -> row + transcript (CASCADE)
```

`tasks` becomes `sessions`; `task_id` becomes `session_id` on the wire; the
SDK's resume id becomes `agent_session_id` to free the name.

Three properties follow, and each replaces a mechanism rather than removing one:

- **A session with no message has no pod.** That is the human gate,
  structurally: nothing machine-initiated produces a message, so nothing
  machine-initiated produces a pod. It replaces `status='proposed'` being
  invisible to a `SELECT`.
- **A session with no message is a valid resting state**, so an optional first
  message needs no special case. This matters concretely: the Agent SDK's
  streaming-input generator is not entered until an input arrives, so a pod
  started with nothing to do never emits `session_id`, is unresumable, and is
  killed by the startup-stall sweep with nothing logged anywhere.
- **Ordering is warm-then-append, never append-then-warm.** `resumeFromSeq` is
  `LatestSeq` computed at dispatch, so a message appended before the pod exists
  lands *below* the pod's cursor and is never delivered. `Discuss` already does
  this correctly and documents it; the new path reuses it rather than
  reinventing it.

### 2. No queue, no lease, no retry, no status

Deleted: `core/internal/dispatch/` entirely, `ClaimNextTask`, `RefreshLease`,
`SetStatus`, `Retry`, `SoftDelete`, and the columns `status`, `heartbeat_at`,
`retry_count`, `claimed_by`, `notes`, `kind`, `external_key`, `guidance`,
`pr_url`, `deleted_at`.

`lease_id` **stays**. It is what stops a torn-down pod still finishing its
shutdown from overwriting the resume identity of the pod that replaced it, and
it gates the pre-push `StillHoldsLease` check. One column and one `WHERE`
clause; it is minted by `SendMessage`/`WarmSession` instead of by a claim.

Liveness comes from Kubernetes. A new 60-second reconcile loop in `core` lists
worker Jobs from the provisioner and writes `pod_phase` to match reality. A
dropped pod event self-heals in 60s instead of never — which the heartbeat
never achieved, because `heartbeat_at` was refreshed by the worker's own timer,
not by evidence the work was progressing.

**One prerequisite, and it is not optional:** `gcTerminalWorkerJobs` today
reports `POD_PHASE_CRASHED` only for a `Failed` Job; a `Succeeded` Job is
deleted with no event at all. Terminal status is currently the only other
`TearDownSession` trigger. Deleting status without fixing this leaves every
finished session at `pod_phase = RUNNING` forever, and since `CountLivePods`
counts exactly those phases, **the fleet wedges permanently after five
successful sessions** — invisibly, until session six.

### 3. Proposals are a separate table, not a status

`proposals` has no pod path. `alertwebhook` and `audits` write it; a human turns
one into a session via `OpenFromProposal`, a `DashboardService` RPC behind
Traefik basic-auth.

This is the same guarantee ADR-0029's `ApproveProposal` provided for
cluster-access thot tasks — relocated, not deleted. An import-graph test was
considered and rejected as theater: `alertwebhook` and `audits` already declare
narrow interfaces with the concrete store injected in `run.go`, so such a test
passes whether or not they are wired to the opener. The guarantee is the table
split plus the auth boundary, both of which are checkable by inspection.

Dedup is keyed `(repo, dedup_key) WHERE dismissed_at IS NULL`. Keying it on
"has no session yet" would free the key the moment a human opens the proposal,
so a 1-hour audit cadence whose session runs 3 hours yields three proposals.

### 4. Storage is split by access pattern, not by uniformity

**This section was rewritten after measuring. The original decision — one
shared RWX PVC on a new `nfs` StorageClass, holding everything including
`node_modules` — was wrong, and the numbers say so unambiguously.**

Measured 2026-08-14 in a scratch namespace, one pod on `k8s-worker-01`
mounting all three volumes so node and CPU are identical and storage is the
only variable. Longhorn is measured at its best case: its share-manager runs
on that same node.

| | Longhorn RWX | NFS VM | **emptyDir (node-local)** |
|---|---|---|---|
| Sequential write, 200 MB | 19,050 ms — 10 MB/s | 18,297 ms — 10 MB/s | **186 ms — 1069 MB/s** |
| Create 5,000 files | 37,872 ms | 34,200 ms | **182 ms** |
| Delete 5,000 files | 1,589 ms | **15,037 ms** | **77 ms** |
| `bun install` | killed at >3 min | killed at >3 min | **2,375 ms** |
| fsync latency | 9,125 µs/op | 5,205 µs/op | — |

Three findings, in order of consequence:

**The NFS StorageClass earns nothing.** 10% faster file creation, 9.5× *slower*
deletion, identical bandwidth. The premise — remove Longhorn's 3-replica
synchronous fan-out and installs get much faster — is refuted. The fan-out is
real (fsync 9.1 ms → 5.2 ms) and it is swamped. **The fleet stays on
`longhorn`.** The NFS VM remains available as a cluster capability; nothing
here needs it.

**The bottleneck is the fabric, not the backend.** Two independent storage
systems on different physical hosts both cap at exactly 10 MB/s ≈ 80 Mbps,
which is 100BASE-TX after overhead. Not confirmed from inside the cluster
(the nodes are VMs; virtio reports no link speed) — `ethtool` on the PVE hosts
would settle it. Either way, no choice between network storage backends can
fix it.

> **Measured 2026-08-15. The conclusion holds; the host was the other one.**
>
> `ethtool` on the PVE hosts — the check named above, finally run:
>
> | host | link | notes |
> |---|---|---|
> | **`.165` (bnei)** | **100 Mb/s** | the Ryzen box |
> | `server1` | 1000 Mb/s | RTL8153 on a 5 Gb/s USB 3 port; onboard NIC dead, no carrier |
> | `ex-laptop` | not yet measured | |
>
> A first pass at this note asserted the opposite — that server1 and ex-laptop
> were the 100 Mbps hosts, on the strength of a recollection rather than a
> measurement. `lsusb` then showed server1's adapter is a **gigabit** RTL8153,
> and `ethtool` put it at 1000 Mb/s. Recorded here because the wrong version was
> committed before anyone ran the command that was sitting in this very
> paragraph.
>
> `.165` explains every number in the table above, because it is in every path:
> the benchmark pod ran on **`k8s-worker-01`, which is a `.165` guest**, and the
> Longhorn replicas span all three hosts. It also explains an observation from
> outside this repo — a PVE **VM migration** between hosts caps at the same
> ~10 MB/s, and that shares no code with Longhorn or NFS: no replicas, no fsync,
> no `sync` export. Three unrelated workloads, one shared link.
>
> 80 Mbps is 100BASE-TX after TCP overhead, so the original inference was right
> about the mechanism and wrong only about which machine.
>
> **Cause found.** `.165`'s own NIC is gigabit (onboard RTL8168H, advertising
> `1000baseT/Full`, zero errors). It reaches the Freebox through an extra hop
> that the other two hosts do not have — a TP-Link device whose autonegotiation
> advertises `10baseT` and `100baseT` only, no `1000baseT/Full`. A ~€15
> unmanaged gigabit switch replaces it. See `infra-bootstrap`'s
> `docs/infrastructure-actual.md` for the full diagnosis; it is that repo's
> concern, not this one's.
>
> Two consequences worth stating plainly, because both are easy to re-derive
> wrongly later:
>
> - **The `nfs` StorageClass is not a fleet performance lever.** ADR-0036 stands
>   on its own merits (RWX without a share-manager pod, capacity relief on the
>   scarcest disks); it is simply not this problem's answer, and a plan to move
>   the fleet onto it should not be written again.
> - **No storage hardware would help either.** 1069 MB/s node-local is the same
>   disks, on the same day. The disks were never the constraint.
>
> Fixing `.165`'s link is worth doing for etcd stability, image pull time and
> Longhorn rebuild time — it carries `k8s-worker-01`, `k8s-cp-01`, the Postgres
> leader and the Garage S3 node. It is **not** what makes the fleet fast: even
> at gigabit, network storage stays two orders of magnitude below node-local,
> which is why the split below went in first and independently of it.

**Node-local disk is ~200× faster on metadata and 107× on bandwidth**, and
that reframes docs/adr/0039's 782-second cold `bun install` entirely. That was
never a Longhorn problem; it is a *network-storage* problem, and putting
`node_modules` on any shared volume is the design error. The same install
finishes in **2.4 seconds** on the node's own disk.

So the split is by access pattern:

```
/workspace          per-session `local-path` PVC   node-local, durable, auto-pinned
/cache              same PVC, subPath "cache"      node-local, warm across warms
/repo-cache         longhorn RWX, read-only        the clone cache — small, read-mostly
/claude-home        longhorn RWX, per-session      resume state — must survive node loss
```

**Amended during implementation: `/cache` is a subPath of the session's own
PVC, not a per-node one.** The original line said "per-node `local-path` PVC,
warm across sessions on that node" — a shape Kubernetes does not have. A
`local-path` PVC binds to exactly one node, so sessions scheduled anywhere else
cannot mount it, and `WaitForFirstConsumer` means the node is not known until
after the pod is scheduled, so the PVC cannot be named per-node up front. The
only real ways to get a genuinely per-node directory are a `hostPath` mount on
every session pod (which a restricted PodSecurity profile forbids) or a
DaemonSet-managed volume, and neither is worth it here.

What is lost is cross-session cache sharing: a brand-new session installs cold.
What is kept is the part the measurements actually bought — node-local disk, so
that cold install is the 2.4-second one rather than the >3-minute one — and the
cache stays warm across every warm of the same session, since the PVC is pinned
and reused. Sharing was never the win; the fabric was the problem.

Node-local for anything write-heavy and regenerable; replicated network
storage only for the small things whose loss actually matters.

**`local-path` per session rather than `emptyDir`, and the pinning is the
point.** `WaitForFirstConsumer` binds the volume to whichever node the first
pod lands on, and the volume's own node affinity then *forces* every
subsequent warm back to that node — so a resumed session finds its working
tree and its warm dependency cache already there, with no affinity rules to
write and no re-clone. GC becomes `kubectl delete pvc`.

An earlier draft of this ADR rejected per-session `local-path` precisely
because it pins. That objection inverts once the measurements are in: pinning
is the mechanism that delivers both node-local speed and cache locality.

The cost is real and accepted: **a drained node strands its sessions** until
it returns, and `drain-self.service` runs on two of the five nodes. The
mitigation is that git is the durable copy — which at 10 MB/s of local
replication is the only replica worth having anyway.

`/cache` stays **global across repos, per node** — the personal-computer
model. Go's module cache, bun's and npm's are content-addressed with atomic
renames and designed for concurrent access. The correction to the original
design is not that sharing was wrong; it is that a personal computer's
`~/.bun/install/cache` sits on its own SSD, not an NFS mount.

Isolation becomes a real mount boundary for the first time. `claude-home/<id>`
being per-session kills the mid-flight `rsync --delete` problem outright:
`SyncFleetShared` seeds it once at pod creation, never while a session is live.

**`claude-home/<id>` being per-session is load-bearing, not tidiness.** The SDK
derives its per-project directory from `cwd`, replacing every non-alphanumeric
character with `-` — verified against a real installation, e.g.
`projects/-Users-moha-Code-infra-bootstrap-agent-fleet--claude-worktrees-session-rewrite/`.
Today every session's `cwd` is `/workspace/worktrees/<taskId>`, so those
directories are naturally disjoint. **After the merge every session's `cwd` is
`/workspace`, so every session encodes to the same `projects/-workspace/`.** The
per-session mount is what keeps them apart; without it, resume state from
different sessions would land in one directory.

A session's SDK state is also **three** paths, not one — confirmed by
inspection rather than inference:

```
projects/<encoded-cwd>/
  <agent_session_id>.jsonl        the transcript (3.9 MB for one real session)
  <agent_session_id>/subagents/   subagent transcripts
```

An earlier draft of this decision had the sweep delete only the `.jsonl`,
which would have leaked the `subagents/` directory on every sweep. Sweeping
the whole `claude-home/<id>` subtree avoids needing to know the layout at all,
which is the point — the fleet should not be tracking the SDK's internal
filenames.

A draft of this ADR moved the backing class from Longhorn RWX to a plain NFS
export, reasoning that Longhorn's replica fan-out was the part of the 782s a
single NFS server does not have. That reasoning was tested and was wrong —
see §4's table. The fan-out is real but minor; the fabric is the constraint,
and both options share it. The class stays `longhorn`.

### 5. The fleet leaves the git business

**Amended during implementation — §4 and §5 were in direct conflict.** §4 was
rewritten after the storage measurements and §5 was not updated with it. §5 had
the provisioner run the per-session clone; §4 put the session's working tree on
a per-session `local-path` PVC. The provisioner cannot write into that volume:
`WaitForFirstConsumer` means it does not bind until the session pod is
scheduled, and even after it binds the provisioner does not mount it. Nothing
would have failed at build time — the clone would simply have landed on the
provisioner's own view of a different volume.

The resolution is an **init container in the session pod**, which is the only
place both volumes are mounted at once:

```
provisioner, on the shared RWX volume:
  EnsureRepoCloned(repo)              # fetch the clone cache, gc.auto=0
  seed claude-home/<id> from fleet-shared

session pod, init container, before the sidecar:
  git clone --shared /repo-cache/<repo> /workspace
```

This is better than what it replaces, not merely a workaround: the clone now
runs *on the node that owns the volume*, so it is node-local at 1 GB/s rather
than crossing the 10 MB/s fabric, and the provisioner is out of the git
business entirely rather than almost.

`claude-home/<id>` stays a subPath on the shared RWX volume rather than a PVC
of its own — it must survive node loss (it is the resume state), the
provisioner has to seed it before the pod exists, and a per-session Longhorn
PVC would mean three replicas apiece for a few megabytes.

No worktree, no branch, no naming convention. The agent runs `git checkout -b`
and `gh pr create` itself, as it already does for the PR.

`--shared` writes an `alternates` file into the read-only cache, so objects are
never copied and the clone is near-instant. **`gc.auto=0` is set on the cache
clones** — a `git gc` there can prune objects a live session references, a
documented git hazard and the one sharp edge this approach has.

Deleted: `CreateWorktree`, `DeleteWorktree`, `ListWorktrees`,
`SweepGoneBranches` and its two accepted leak gaps, `provisioner/internal/sweep/`,
and the `agent/<id>` convention.

GC becomes `rm -rf sessions/<id>` and `claude-home/<id>`, on archive or after
`SESSION_RETENTION_MS` (default 14d) idle, then `swept_at`. `core` is now
genuinely the single writer of that fact — today `SweepGoneBranches` deletes
worktrees on the provisioner's own ticker without telling anyone, so any
"session knows whether its disk exists" claim would be false.

ADR-0023's rule — never delete a worktree as a side effect of a terminal status
— is retired rather than kept, because it exists only to protect worktrees the
fleet owned. It owns none.

### 6. The sandbox merges into the worker

One pod per session, holding the agent and its app.

Deleted: `e2e-runner/` as a separate image, `execmcp`, `run_command` and its
retry ladder, `request_e2e_env`, `kill_env`, `sidecar/internal/e2eclient/`,
`ServiceEndpoint` + `FLEET_ENDPOINTS`, `playwright_tools.json` +
`addProxiedTool`, the per-task NetworkPolicy, five e2e RPCs, `gcDeadE2ePods`,
and the whole recipe system.

Builds and tests are native `Bash`. The un-prompted set moves to allow-rules in
`fleet-shared/settings.json` — the same file a CLI user edits — rather than
being a property of which pod the command lands in.

`allowedTools` in the `query()` call **stays**, and this is worth recording
because the opposite was proposed and was wrong. The SDK's MCP tools return
`behavior: "passthrough"`, and its permission evaluator converts passthrough to
`ask`. Deleting `allowedTools` would gate `send_message` and the MCP
`AskUserQuestion` — the agent would need permission to ask for permission. Only
the nine explicit `mcp__agent-fleet-sidecar__<name>` entries go; the
`mcp__agent-fleet-sidecar__*` wildcard beside them already covers every one.

Playwright runs in-pod on localhost as a second `mcpServers` entry — no proxy,
no snapshot, no drift.

Two new MCP tools, both routing agent → sidecar → `core` → provisioner so the
pod stays RBAC-free:

| Tool | Does |
|---|---|
| `expose(port)` | Service + IngressRoute on `<id>.e2e.bnei.dev`, returns the URL |
| `unexpose()` | Removes them |
| `request_service(kind)` | Provisions/reuses a shared Postgres or Redis, returns a connection string |

`request_service` is the one capability that stays fleet-side, because it needs
cluster RBAC and the agent has none. Everything else in the old recipe system
was the fleet reading the repo on the agent's behalf.

`repos.image` — one column replacing three tables — names the image a repo's
sessions run.

> **Amendment, v3.3.x.** This shipped only as far as Postgres. The column, its
> store round-trip and its dashboard input all landed with this ADR, but
> `CreateWorkerPodRequest` had no `image` field, so the value never left
> `core` and `pod.go` took the provisioner's `WORKER_IMAGE` unconditionally.
> For three minor versions, editing the field in "manage repos" did nothing —
> and nothing errored, because every layer was individually correct and none
> was connected to the next. The four toolchain ingredients this column
> replaced stayed in `catalog.Tools` that whole time, with `go-toolchain`
> still putting `/opt/tools/go/bin` ahead of the image's own Go on `PATH`.
>
> The field now exists and applies to the **worker container only** — the
> clone init container and the sidecar stay on the fleet's own images, since
> cloning needs `git` and the sidecar is a fleet binary at a fixed path, and a
> repo-supplied image owes neither. The dead ingredients are deleted. Guarded
> by `TestCreateWorkerPod_RepoImageAppliesToTheWorkerContainerOnly` and
> `TestTools_CarriesNoToolchain`.

## Alternatives considered

- **Keep the queue, delete only the dead columns.** Rejected: `pending` only
  pays off if more than five sessions are fired and left unattended. The two
  machine-initiated paths are proposals now, and proposals never auto-open.
- **Keep a terminal status.** Rejected: sessions are polymorphic — a bug fix, a
  multi-agent feature, an explanation. There is no shared completion criterion
  to compute, which is precisely why `done` never acquired a writer.
- **Delete `lease_id` along with the rest of the lease machinery.** Rejected on
  adversarial review: teardown is not synchronous, the volume is shared, and
  the session directory is keyed by id — overlapping pods are the routine case.
- **Delete `allowedTools`.** Rejected: see §6. The stated justification (that
  `default` mode auto-allows reads) is false for MCP tools.
- **An `approved` boolean on the session row.** Rejected: a flag on a row that
  also has a pod path is a gate to bypass; a separate table has no pod path.
- **Keep `description` as the prompt.** Rejected: a polymorphic session's
  description conveys nothing shared, so it cannot earn a place in the context.
  It survives as a UI label that never enters the prompt.
- **Keep the executor sandbox, merge nothing.** Rejected: one un-gated tool does
  not justify a pod, an image, a NetworkPolicy, an endpoint roster and three
  repair ADRs — particularly when the property it protects is one click deep.
- **Two containers in one pod.** Rejected: keeps ADR-0044's crash containment
  but also keeps the fat image split, *and* still requires the platform to know
  how to restart the app — i.e. the recipe system survives, which is most of
  the complexity.
- **`local-path` per-session PVC.** Rejected in draft on the grounds that
  `WaitForFirstConsumer` binds each session to one node for its whole life —
  then **adopted** once measured. The pinning is the mechanism, not the flaw:
  it is what returns a warmed session to its own working tree and its own warm
  dependency cache, with no affinity rules. The drain risk is real and
  accepted; see Consequences.
- **A node-local `emptyDir` working tree.** Rejected: it dies with the pod, so
  every warm re-clones over a 10 MB/s fabric — and losing uncommitted work at
  the 30-minute idle timeout is the failure `docs/DECISIONS.md` records
  happening twice already. `local-path` is node-local *and* durable.
- **One shared RWX PVC holding everything, including `node_modules`.**
  Rejected on measurement: ~200× slower metadata than node-local disk, and it
  is the actual cause of docs/adr/0039's 782-second cold install.
- **An NFS-backed StorageClass to escape Longhorn's replica fan-out.**
  Rejected on measurement: 10% faster creates, 9.5× slower deletes, identical
  bandwidth. See §4.
- **Auto-WIP-commit on teardown.** Rejected: writes commits to the agent's
  branch behind its back, and a failed push (conflict, no upstream, no network)
  has nowhere to report.
- **Per-repo caches, as today.** Rejected: it is the one thing a developer's
  machine unambiguously does the other way.
- **Discord buttons answering permissions.** Rejected: the dashboard is behind
  Traefik basic-auth and the Discord handler has no user allowlist; an
  unanswered `AskUserQuestion` re-asks every 60s with a fresh `seq`, so every
  prior message's control is silently dead; `custom_id` caps at 100 characters
  while the answer payload is keyed by full question text; and
  `permission_request` entry text is `JSON.stringify({tool, input})`, so
  relaying it leaks file contents and command lines into a channel. Discord
  becomes notification with a deep link.

## Consequences

**Accepted costs, stated plainly:**

1. **A malicious postinstall script can reach `GH_TOKEN` and the Claude OAuth
   token.** The sandbox contains this today. Bounded by: a human approved the
   command that ran it, and the agent already had gated `Bash`/`Write`/`Edit`
   on the credentialed pod.
2. **The preview URL dies with the session's pod** — Stop, 30-minute idle, or
   crash.
3. **Builds will prompt** until allow-rules are added to
   `fleet-shared/settings.json`. This is a behaviour change, not a regression:
   it moves the decision from "which pod" to "which rule", where a human can
   see and edit it.
4. **Crash containment moves to the agent.** ADR-0044's incident was a dev
   server taking PID 1. In one pod, `fleet-shared/CLAUDE.md` documents
   `setsid`/`nohup` and a log path, and the agent is responsible for it.
5. **A session's working tree lives on one node's disk, unreplicated**, so a
   drained or dead node strands or loses it — and `drain-self.service` runs on
   two of the five nodes. Git is the real replica; the exposed window is the
   tree between commits. Accepted because the alternative costs ~200× on every
   metadata operation, on every session, forever.
6. **`buf breaking` fails by construction** on the `task_id` rename. The only
   opt-out is a literal `BREAKING CHANGE:` footer, which makes `release-it` cut
   a major version. Both are intended.
7. **The database is purged.** The 14 migrations squash to a fresh `000001`.
   The down migration must be real, not a stub — a failed PreSync leaves the DB
   dirty and gates every subsequent core deploy.

**What gets simpler, concretely:** the dispatch package, the claim query, the
lease machine, the worktree lifecycle, the branch sweep, three profile tables,
the recipe resolver, the endpoint roster, the MCP proxy, `execmcp`, a whole
image, a whole pod per session, and 8,692 lines of ts-proto output in
`worker/src/gen/` that nothing imports.

**What this does not decide:** Loki/Grafana observability beyond adapting
ADR-0047's gauge labels; Garage S3 shared files (ADR-0031); inter-agent tools
(ADR-0041), which are renamed only; context-budget caps (ADR-0046); and the
dashboard's visual design (ADR-0042/0043), where only the data model changes.

## Provenance

This decision came out of two architecture interviews and one adversarial doubt
cycle — three fresh-context reviewers against an earlier draft, which found
three fleet-stopping errors in a plan that read clean. The most consequential:
deleting `allowedTools` would have gated the sandbox's own tool; deleting
terminal status would have wedged the fleet after five sessions; and appending
the first message before the pod existed would have placed it below the pod's
resume cursor, so no session would ever have received its instruction. Each
had a code comment nearby explaining why the mechanism existed.

That is the reusable finding, and it is the same one the Context section opens
with: **a mechanism with a comment explaining itself is load-bearing until
proven otherwise.** The deletions in this ADR are the ones that survived being
attacked.
