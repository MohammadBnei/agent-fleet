# ADR-0039: The e2e pod is the worker's execution sandbox

**Status:** Superseded by [0048](0048-one-session-one-pod-one-shared-home.md) — the sandbox merges into the worker pod. The un-gated `run_command` it existed to justify becomes native `Bash` under allow-rules, and the isolation it bought was one approved command deep
**Date:** 2026-08-12
**Builds on:** [ADR-0012](0012-e2e-provisioner-standalone-app.md) (the RBAC
boundary and the code-server denial), [ADR-0034](0034-environment-recipe-system.md)
(what's actually installed in the pod), [ADR-0036](0036-e2e-recipe-visible-and-override-approved.md)
(the readiness probe this partly corrects).

## Context

Two pods exist per task, and shell commands were running in the wrong one.

The **worker pod** holds `GH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` in its
environment, mounts the **whole** workspace PVC (every concurrent task's
worktree — deliberately, since a linked worktree's gitlink is an absolute
path into `repos/<repo>/.git/worktrees/`), and is selected by no
NetworkPolicy. It is a thin Bun container: `git`, `gh`, `curl`, and the
Agent SDK.

The **e2e pod** is where a repo's actual environment lives. ADR-0034 gave it
the resolved profile's toolchain, its Postgres/Redis, and a shared
dependency cache; ADR-0038 gave it a preview URL. It holds no fleet
credentials, mounts only this task's worktree via `SubPath`, and is the
single pod in the cluster under a NetworkPolicy.

So the pod with the compilers had no reason to be trusted, and the pod with
the credentials had no compilers. The agent built and tested in the latter.

Most of the connective tissue for fixing that already shipped, un-ADR'd,
with issue #65: `e2e-runner/cmd/execmcp/main.go` serves a `run_command` MCP
tool (`bash -lc`, cwd = worktree), `provisioner/internal/mcpproxy` routes it,
and `CallE2eTool` passes it through core under ADR-0020's hub-and-spoke rule.
Three things stopped it being usable as a sandbox:

1. **It only existed after `request_e2e_env`.** Registration happened inside
   that handler, announced via `notifications/tools/list_changed` — a
   mechanism ADR-0012 flagged as unverified against the Agent SDK and nobody
   since confirmed. A **resumed** session (ADR-0029's warm path) never
   re-registered it at all, because the agent had no reason to re-request an
   environment that was already running.
2. **The exec port was unreachable exactly when it was needed.** The e2e
   `Service` had no `publishNotReadyAddresses`, and ADR-0036 added a
   `ReadinessProbe` on `AppPort` — so whenever the target app wasn't
   listening the pod's endpoint was marked `ready: false` and kube-proxy
   routed nothing to it, and `ExecURLFor` addresses the Service. A pod whose
   `start_cmd` is broken is precisely when you want a shell, and precisely
   when you could not get one.

   Measured live on `ukubi-cluster` (2026-08-12) rather than reasoned about:
   one pod listening on the exec port and nothing on the app port, behind
   two Services differing only in this field. Through the
   `publishNotReadyAddresses` Service the exec port returned **HTTP 200**;
   through the plain one the identical request was **unreachable**. Note the
   endpoint is present in the `EndpointSlice` in both cases — it is the
   `ready` condition that differs (`false` vs. forced `true`), not slice
   membership.
3. **The command timeout was 5 minutes**, against a cold `bun install` that
   ADR-0036 measured at **782 seconds** on the shared cache. The one command
   the sandbox most exists to run was longer than its own timeout.

## Decision

### `run_command` is a static sidecar tool, present from the first turn

Registered in `sidecar/internal/mcpserver`'s `New()` alongside
`send_message`/`AskUserQuestion`/`request_e2e_env`, not lazily. It exists on
turn one of a fresh session and on turn one of a resumed one, with no
dependency on `notifications/tools/list_changed` and no dependency on the
agent having called anything first.

`ProxiedTools` (`provisioner/internal/mcpproxy`) stops advertising it, and
the sidecar's dynamic-registration pass skips it by name. Both are needed:
the two run as separate images, so a rolling deploy has a real window where
an old provisioner still lists it and would overwrite the static handler
with a plain passthrough.

### The sandbox is provisioned lazily, on the first command that needs it

The handler calls `CallE2eTool` first and only provisions on failure.
`execmcp` reports a nonzero exit as an ordinary result rather than an error
(a failing build is information, not a transport fault), so an error from
that call means one thing: no live pod. The handler then calls
`RequestE2eEnv` — already idempotent, `CreateE2eSession` short-circuits on
an existing pod — and retries once.

Provisioning up-front on every call would also have been correct, but puts
two extra gRPC hops and a Postgres profile lookup in front of every command
to fix a state that exists once per session. Doing it on failure also
self-heals after `kill_env`, which a provisioned-once flag would not.

The retry's result carries the resolved recipe (`profileName`,
`resolvedStartCmd`, `tools`, `services`) as a leading text block — the same
blindness ADR-0036 added `resolved_start_cmd` to `request_e2e_env` to fix.
Otherwise a sandbox appears from nowhere with an unknown toolchain.

### `git` stays out of the sandbox, and that is the security control

The `SubPath` mount severs the linked-worktree gitlink, so `git` fails in the
e2e pod. This is kept, not fixed. ADR-0012 built the code-server
NetworkPolicy denial for one stated reason — code-server is "a full
IDE/terminal … that would otherwise let the agent bypass the whole
git-worktree → commit → PR flow." A git-less shell cannot bypass that flow.
`git`, `gh`, and everything that opens the PR stay on the worker pod's own
`Bash`, and the tool description says so explicitly.

### `run_command` stays outside `canUseTool`, unlike `Bash`

`worker/src/session.ts`'s `mcp__agent-fleet-sidecar__*` entry in
`allowedTools` covers `run_command`, so it is never prompted — while the
agent's native `Bash` is gated per call. That asymmetry reads backwards and
was previously undocumented, which is why it needs recording rather than
quietly inheriting.

It is deliberate: the e2e pod is *strictly less privileged* than the pod
whose shell is gated. No `GH_TOKEN`, no `CLAUDE_CODE_OAUTH_TOKEN`, one
task's worktree instead of all of them, no git, and the only NetworkPolicy
in the cluster. Prompting on the weaker surface while leaving the stronger
one open would buy nothing and would put a human click in front of every
`go build`. If the balance ever changes — credentials added to the e2e pod,
or the `SubPath` mount widened — this decision has to be revisited, because
those are the premises it rests on.

### `publishNotReadyAddresses` on the e2e Service

Corrects the reachability half of ADR-0036. `app_ready` on the dashboard
card is unaffected — it reads pod conditions via `GetPod`, never endpoints —
so nothing loses the visibility that probe was added for. This also restores
code-server reachability on a pod whose app never binds, which is the exact
outcome ADR-0036 chose readiness over liveness to get and did not achieve.

### The sandbox is sized for building, not just previewing

`250m`/`512Mi` requests, `2000m`/`2Gi` limits → **`1000m`/`1Gi` requests,
`4000m`/`4Gi` limits**. The request is the more important half: it sets the
CFS weight, so the old `250m` meant a pod got a quarter core to install
dependencies with whenever the node was busy. The 4-core limit is burst
headroom for the common idle-node case, not a reservation — at the
concurrency cap of 5, sandboxes reserve 5 CPU / 5Gi against
`k8s-worker-01`+`k8s-worker-02`'s 12 cores.

This required raising `limitRange.max.cpu` from the `common-app-chart`
default of `"2"` in `k8s/core.yaml`. That LimitRange is **namespace-wide,
not release-scoped**: core's release creates it, and it governs the pods the
*provisioner* creates. The e2e pod's CPU limit had been sitting exactly on
that ceiling since it was written, and a container limit above `max` is
rejected at admission — the pod is never created rather than merely
throttled.

Confirmed live rather than assumed: `agent-fleet`'s LimitRange on
`ukubi-cluster` reads `max.cpu: "2"` today, and creating this pod shape
against a mirror of it in a scratch namespace failed with
`Forbidden: maximum cpu usage per Container is 2, but limit is 4`. Raising
the mirror to `"4"` let the identical pod schedule. **The resource bump
without the `core.yaml` change would have stopped every e2e pod from being
created at all** — which is also why the two are pinned together by a test. Two files in different languages and deploy paths that must move
together, so `TestCreatePod_ResourcesWithinLimitRange` parses `core.yaml`
and pins them (an outside-the-module read, like `core/internal/buildguard`,
so run it with `-count=1`).

**This is not expected to fix a slow `bun install` on its own, and the
measurement should decide what does.** The workspace PVC is Longhorn RWX —
an NFS share-manager in front of a 3-replica volume — and both
`BUN_INSTALL_CACHE_DIR` (`/cache/bun`) and the `node_modules` being written
live on it. A dependency install is tens of thousands of tiny file
creations, i.e. tens of thousands of NFS round trips each fanning out to
three synchronous replica writes. That is a metadata/IO-bound workload, and
no amount of CPU moves it. If the measured time doesn't drop materially,
the next lever is putting `node_modules` on a pod-local `emptyDir` rather
than the shared PVC — not more cores. Longhorn's own RWX/NFS tuning is
`infra-bootstrap`'s call, not this repo's.

Sized against the measured 782s cold install, with headroom. Nothing
upstream bounds this call — core's `sessionCallTimeout` covers
`CreateWorkerPod`/`TearDownSession` only.

## Alternatives considered

- **Redirect the agent's `Bash` tool into the e2e pod entirely** (disallow
  the local one). Rejected: the worker pushes commits and opens the PR from
  Bash mid-session (`worker/src/index.ts`'s `configureGitAuth` exists for
  exactly that), and git does not work in the e2e pod. Making it work means
  the whole-PVC mount, which hands the sandbox every other task's worktree
  and removes the isolation that justifies leaving `run_command` un-gated.
  The two changes cancel each other out.
- **Create the sandbox at worker-pod start, always.** Rejected: a full e2e
  pod per task including doc-only ones, and it requires an `"e2e"` profile
  on every repo. Lazy provisioning costs one slow first call, on the command
  that was going to be slow anyway.
- **`pods/exec` RBAC on the provisioner** instead of an in-pod MCP listener.
  Rejected here for the third time (ADR-0012, ADR-0029): Kubernetes RBAC
  cannot scope to "this task's own pod", so any grant is fleet-wide, and
  `exec` is a live shell regardless of intent.
- **A second Service, or pod-IP addressing, for the debug ports** instead of
  `publishNotReadyAddresses`. Both work and both keep the app route strictly
  ready-gated; rejected as more moving parts than a field that also fixes
  code-server. The cost is that Traefik returns a refused-connection 502
  instead of a 503 while the app is starting — two failure pages.
- **Gate `run_command` through `canUseTool` for symmetry with `Bash`.**
  Rejected on the privilege argument above, and because a human click in
  front of every build makes the sandbox worse than the thing it replaces.

## Consequences

- The agent has an always-available shell in a credentialed-free pod, and
  the tool description steers builds/tests/lints/installs there while
  keeping git on `Bash`. Whether that steering actually holds is a prompt
  outcome, not an enforced one — nothing prevents the agent from running
  `go build` locally.
- **e2e pods are still never garbage-collected**
  (`provisioner/internal/reconcile/loop.go`, a pre-existing acknowledged
  gap). Lazy provisioning creates them on a new trigger, so more will exist.
  They still tear down on terminal task status, stop-grace, and `kill_env`.
- The `run_command` description now lives in two places, not three:
  `sidecar/internal/mcpserver` (what the agent sees) and
  `e2e-runner/cmd/execmcp` (what the server advertises). The proxy's copy is
  gone. They can still drift.
- A repo with no `"e2e"` profile gets a clear failure from the first
  `run_command` rather than a missing tool — `RequestE2eEnv` surfaces the
  profile-resolution error into the tool result.
- Two bugs ADR-0036 named remain untouched: Playwright MCP binds `::1:8931`
  so its Service port routes to nothing, and a target app owning `/code` is
  shadowed by the code-server route.
