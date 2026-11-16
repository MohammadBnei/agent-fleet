# ADR-0085 — Overlay session volumes, built on the node; a warm pool only if measurement still asks

- **Status:** Proposed (2026-11-16). Owner decisions from an architect-companion round the same day,
  after a survey of agent sandboxes on Kubernetes and a measurement of today's warm path.
- **Date:** 2026-11-16
- **Built:** nothing.
- **Would amend:**
  - [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §4 (storage split by access pattern) and
    §5 (the fleet's git lifecycle): a session's working tree becomes an overlay over shared read-only
    layers instead of a full clone on its own PVC.
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §3 (the token is minted before
    the Job exists), **only if** the warm pool (§6) is built.
- **Related:** [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) (core commands, the
  provisioner executes), [ADR-0084](0084-long-lived-agents.md) (long-lived agents, proposed
  alongside).

## Context

**Measured 2026-11-16** (Tempo, eight `CreateWorkerPod` traces over seven days, plus core spans):

| Stage | Time |
|---|---|
| Provisioner `CreateWorkerPod`, typical | 3.4–4.6 s (range 1.1–20.4 s) |
| of which `warm.git.fetch` inline | 0.8–19.9 s (a first-ever clone: 8.9 s) |
| of which fleet-shared sync, repo pods | 3.1 s (a fetch, then `git reset`) |
| everything else (genome, vault mint, PVC, Job) | ≈0.2 s |
| Job create → sidecar's first core call | +1.1 s |
| → worker connected (`GetSession`) | +3.4 s (init clone, Bun boot, worker setup: no spans yet) |
| → SDK session ready (`SaveAgentSessionId`) | +4.7–5.8 s |

Message to a ready agent is about 6–10 s end to end, not the 3–4 s quoted before: that figure was
the provisioner's share only. The owner wants sub-second warms, with no pod started on a guess
(not when a player walks near a resident).

**Why the storage is the hard part.** A pod's volumes are fixed at creation. Today each session's
`/workspace` and `/cache` live on its own `local-path` PVC, pinned to one node on purpose so a resumed
session finds its uncommitted work (ADR-0048 §4, `provisioner/internal/k8s/pod.go:464`), and its SDK
resume state is `claude-home/<session>` on the shared RWX volume (`pod.go:553`). The shared volume
was measured at 10 MB/s, too slow for working trees and dependency caches
(`k8s/provisioner/pvc.yaml`). A generic pod made ahead of time cannot mount a session's PVC later.
The only way found to attach storage to a running pod is a bind mount made on the node into a mount
point that propagates into the container; Kubernetes has no KEP for attaching volumes to a running pod.

## Decision

### 1. A session's workspace is an overlay, built on the node

- `/workspace` is an overlayfs: shared **read-only lower layers** and one **private writable upper
  layer** per session.
  - Lower layers: the repo at a commit, the tool and dependency caches (Bun, Go), and fleet config.
  - Upper layer: the session's own changes, on node-local disk, pinned to its node as today.
- Mounting an overlay takes milliseconds. It replaces the init clone and a cold dependency install,
  on every warm, pooled or not. Sessions share tools and caches without copying them, and grow only
  by their diff.
- Lower layers are materialized on each node's local disk (not read from the 10 MB/s RWX volume).
  A layer is immutable once published; a refresh publishes a new layer, and the next mount uses it.
- `claude-home/<session>` (SDK resume state) stays on the shared RWX volume, unchanged.

### 2. A CSI node plugin builds and binds it

- The pod asks for an inline CSI volume (`late.fleet.bnei.dev`), mounted with `HostToContainer`
  propagation. At pod start the kubelet's `NodePublishVolume` creates an empty, propagating mount
  point; the plugin builds the session's overlay into it when the session is bound (at start for a
  cold pod, at claim for a pooled one).
- **The kubelet owns cleanup.** `NodeUnpublishVolume` runs when the pod goes; no finalizer and no
  crash unmount of our own.
- The plugin is privileged node code in the shape the cluster already runs (Longhorn's and
  local-path's node plugins). It ships from `provisioner/` as its own DaemonSet, because the
  provisioner Deployment must not hold node privilege.

### 3. Pull, not push: a `SessionMount` written only by the provisioner

- **Core decides whether** a session gets compute (`ReserveSlot`, its pools, the lease) and asks
  the provisioner. **The provisioner decides where and how**: it picks the pod, writes a
  `SessionMount` object (`{session, node, pod, layers, upper}`), and owns the layers. Core holds no
  cluster RBAC and writes no CRD (ADR-0020).
- The plugin on each node watches only its own node's `SessionMount` objects. It has no network
  listener. Before it mounts anything it checks: the target pod carries the fleet label and is
  unbound; every path stays under its own root; the session's profile matches the pod's
  (a definition pod never receives a repo session's tree).

### 4. Layers are refreshed off the warm path

- The provisioner keeps the shared layers current in the background: repo mirrors, caches, fleet
  config. A warm mounts the newest published layer and never runs `git fetch` inline.
- Freshness: a warm must still see the base branch's head as of the warm. If the newest layer is
  behind the remote, the provisioner publishes a new one first (the rare path); the common path is a
  cheap head check. The slice that builds this states and tests the rule.

### 5. Sessions talk only through core

No shared writable directory between sessions: that is the forbidden "shared writable volume across
sessions" pattern, and one session could poison another with nothing recording who wrote what.
Sessions communicate through core (`PublishEvent`, the bridge, transcripts), where trust tiers
apply. Read-only sharing of an artifact can be a published layer.

### 6. The warm pool: measure first

- **First:** worker spans (boot, `query()` start, first SDK message), the overlays, and the fetch off
  the warm path. Then measure the cold path again.
- **Only if it is still over target:** a pool of **one generic idle pod per policy class** (definition,
  egress-locked; repo, open egress), **outside the cap of 5**, refilled in the background after each
  claim. Pool pods carry nothing session-specific: no repo uses a custom image today
  (infra-bootstrap `gitops/bootstrap/fleet-repo-overrides.yaml` sets only `clusterAccess`), the
  workspace arrives by `SessionMount`, and the session's inputs arrive over the sidecar's link.
  Cluster-access sessions keep the cold path.
- **The token at claim** (if the pool is built): the provisioner mints it at claim and returns it to
  core, which relays it to the sidecar over the existing lease-authenticated CoreService link and
  keeps it in memory only. This reverses ADR-0065 §3's mint-before-create order for pooled pods.
- A resumed session cannot use the SDK's pre-spawned process (`prewarm()` refuses `resume`), so a
  resume after teardown always pays the CLI start. A message inside the session's idle window
  already reaches a live process.

### 7. Core and the provisioner stay two processes

Core faces the outside (the public console, `AgentService`, Discord, the bridge) and is the only
holder of database credentials. The provisioner faces the cluster (pod, Secret and Job RBAC, the
vault's only client). Merged, one bug in a public process would hold the database, pod creation and
the vault together. The split costs one gRPC hop and an ordered rollout.

## Alternatives considered

- **A privileged DaemonSet that core commands over the network.** Rejected: root with a listener,
  driven by a public-facing process; cleanup and capacity are ours. The CSI frame plus pull (§2, §3)
  keeps the capability and drops both.
- **A pool PVC per slot** (each pool pod brings a fresh PVC the session takes over). Workable and
  unprivileged, but every session still clones and installs into an empty volume; overlays remove
  that work for every warm.
- **A pre-cloned volume pool** (N full clones ready per repo). Rejected: a full copy per session, and
  tools are not shared.
- **Workspaces on the shared RWX volume**, picked by subdirectory. Rejected: every pod would see every
  session's tree (the forbidden pattern), and the volume is too slow for builds.
- **The Agent SDK's `SessionStore`** to make sessions portable. Rejected: the transcript is already
  on shared storage; what pins a session is its working tree, by design. It would also need new core
  RPCs (only core holds database credentials) and tracks the SDK's internal layout.
- **Warming the real pod early on a hint** (a player approaching a resident, a console page opening).
  Rejected by the owner: pods started for nothing.
- **Pausing idle processes.** Rejected: an idle worker already uses no CPU; freezing keeps the memory
  and the slot, and restore-from-disk has no upstream Kubernetes path.
- **Merging core and the provisioner.** Rejected (§7).

## Consequences

- New privileged node code (the CSI plugin), reviewed and golden-tested like the provisioner's Job.
- Session storage partly leaves Kubernetes accounting: overlay uppers are directories the plugin
  manages, not PVCs. The plugin reports capacity (`CSIStorageCapacity`) so the scheduler still sees it.
- A cold warm loses the init clone and cold installs; the inline fetch goes away.
- Overlay semantics apply to the working tree: deleting a lower-layer file leaves a whiteout in the
  upper; `git` sees an ordinary tree.

## The bet

Most of the warm is work we repeat for every session (fetch, clone, install). Doing it once per node
as shared read-only layers, and binding a private upper in milliseconds, gets most of the way to
sub-second without pods kept warm on a guess; a small generic pool covers the rest only if the
numbers still ask for it.

## Not doing

A shared writable directory between sessions; a privileged agent with a network listener; warming on
a hint; gVisor (later, on the future platform); the SDK `SessionStore`.
