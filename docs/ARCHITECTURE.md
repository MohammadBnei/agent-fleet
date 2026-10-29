# ARCHITECTURE

Canonical topology and current features for `agent-fleet` — the WHAT. For
the WHY behind any specific choice, see [`DECISIONS.md`](DECISIONS.md) and
[`adr/`](adr/README.md). This revision reflects
[`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md), which is the
largest single simplification the fleet has had:

- **A session is the unit, and it has ONE pod.** The e2e sandbox, the recipe
  system and the worktree model are all deleted. A session's tree is a
  `--shared` clone on its own node-local PVC, made by an init container in its
  own pod; the fleet does not create working trees or name branches.
- **There is no queue.** No `tasks.status`, no lease-and-reclaim, no retry
  counter, no dispatch loop. `CreateSession` makes a row; the FIRST MESSAGE
  boots the pod. Liveness is reconciled against Kubernetes, not a heartbeat.
- **Builds are plain `Bash`**, un-prompted via allow-rules in
  `fleet-shared/settings.json` rather than by running in a credential-free
  second pod. What survives fleet-side is `expose`/`unexpose` and
  `request_service` — the two things needing cluster RBAC.
- **Storage is split by access pattern**, from measurement: node-local for the
  working tree and dependency caches, replicated only for the clone cache and
  the SDK resume state.

Carried forward from
[`adr/0029`](adr/0029-sessions-not-tasks-permission-prompt-not-approval-gate.md):
a worker pod is ephemeral compute attached to a session on demand ("warm"),
and `canUseTool` reproduces the Agent SDK's own CLI-parity permission tiers
(`default`/`plan`/`acceptEdits`/`auto`) via a live per-tool-call prompt, not a
prospective Write/Edit block. `/approve` does not exist. Two answers are the
fleet's own and never reach a human: its own MCP tools, and everything but
`rm`/`sudo` in `auto` ([`adr/0053`](adr/0053-the-gate-is-canusetool-not-a-rule-list.md)).

Where this disagrees with anything else, this file wins for
topology/features — the reading order is `DECISIONS.md` → this file →
`adr/` → source.

Sections 2–7 were brought forward on 2026-08-15, together with the dashboard
catch-up in v3.0.2, and checked against the code again on 2026-10-22 (v6.23.1) and 2026-10-24
(v6.24.0 plus the unreleased bridge consumer),
when §4 and §6–§8 became pointers: code is the truth, and this file keeps
what code cannot say. Where they describe a deleted mechanism they now do so in
the past tense and say what replaced it — the queue, the lease/heartbeat/
reclaim machine, worktrees and the e2e sandbox are all gone, and a sentence
here written in the present tense about any of them is a bug in this file.

## 1. Components

| Component | Role |
|---|---|
| `core/` | Go service: Discord notifications (outbound only — the dashboard is the primary surface) + `CoreService`, core's own gRPC server (agent-facing proxied MCP calls, wrapper-facing housekeeping, and the provisioner's pushed pod-lifecycle event stream) + `sessions.Loop`, a 60s reconcile/sweep pass that replaces the dispatch loop, the heartbeat and the reclaim: it syncs `pod_phase` against what Kubernetes actually has (in both directions), enforces stop-grace, startup-stall and idle timeouts, runs the retention GC, and publishes the live-state gauge + transcript coordination (Postgres) + Loki queries + the dashboard's ConnectRPC API and static SPA + `AgentService`, the API members (`:8080`) and registered clients (`:9091`) drive definition sessions through ([`adr/0066`](adr/0066-agent-service.md), `core/internal/agentsvc`) + the island bundles (`/islands/`, `core/internal/islands`) + the definitions sync, the GitHub repo sync, context-pack rendering and grants (§4) — one binary. **The fleet's sole holder of `AGENTFLEET_DB_*`** (`adr/0020` point 1) and the *only* caller of the provisioner (hub-and-spoke). Zero cluster RBAC. An `activityTrackingStore` decorator wraps every transcript append to maintain the activity clock the idle sweep and the GC both read. |
| `provisioner/` | Go service (`client-go`) — the only component with Kubernetes RBAC (namespaced `Role`, never a `ClusterRole`). Creates a session's `batch/v1.Job` (`BackoffLimit: 0` — a crashed session is a human's decision to warm again, not a retry) and its own `local-path` PVC, and the `Service` + Traefik `IngressRoute` that `expose()` publishes. Maintains the shared clone cache (`git clone`/`fetch`, `gc.auto=0` so a `--shared` clone's alternates can't be pruned out from under a live session) and seeds each session's own `claude-home` and read-only `/genome` (context packs) before its pod exists. It alone holds the GitHub App key and mints installation tokens (`provisioner/internal/github`), and it is Agent Vault's only client (`provisioner/internal/vault`): it reconciles vaults and mints and revokes each warm's vault token ([`adr/0065`](adr/0065-agent-vault-and-the-definition-egress-lock.md), [`adr/0075`](adr/0075-github-app-identity-and-vault-credentials.md)). It does **not** create working trees: that happens in an init container inside the session's pod, the only place both volumes are mounted (`adr/0048` §5). Holds **zero** Postgres credentials; Kubernetes is its durable source of truth. Hosts `ProvisionerService` (core is the only caller) and pushes `PodEvent`s back to core. |
| `sidecar/` | Go — a second container in every session pod (`adr/0020` point 5). Two `localhost`-only surfaces and one outbound gRPC connection to core: an MCP server the Agent SDK connects to (`send_message`, `wait_for_messages`, `AskUserQuestion`, `expose`, `unexpose`, `request_service`, the shared-file tools, `view_logs`, and the inter-agent tools), and a plain HTTP/JSON API for the TS wrapper's control flow (`journal`, `session-id`, `permission-mode`, `telemetry`, `message`, `task`) — including the load-bearing `GET /human-messages` SSE feed that delivers new human input to `streamInput()` live. An independent telemetry loop pushes `git diff --numstat` stats every 5s, decoupled from what the agent does. On a repo pod it is also the worker's `HTTPS_PROXY`: an egress router on `127.0.0.1:9093` (`sidecar/internal/egress`) that sends only `github.com`, `api.github.com` and `api.anthropic.com` to the vault, which injects the real credential, and tunnels everything else direct (`adr/0075` §7). The sandbox it used to reach over MCP (`adr/0045`) no longer exists. |
| `worker/` | The Claude Code worker (TS/Bun — the only JS runtime left, sole host of `@anthropic-ai/claude-agent-sdk`'s `query()`; `src/session.ts`). **Single-shot**: the provisioner hands it one `SESSION_ID`/`TARGET_REPO`/`RESUME_SESSION_ID`/`RESUME_FROM_SEQ` via env (no `LEASE_ID`: only the sidecar talks to core), pointed at a `/workspace` an init container already cloned, with `CLAUDE_CONFIG_DIR` on its own per-session volume. Runs **one continuous `query()` in streaming-input mode**, starting in the mode its session row names (`"auto"` by default, `docs/adr/0061`) and resuming via `resume:` when `RESUME_SESSION_ID` is set. The input queue is deliberately NOT seeded: there is no fleet-composed prompt, so the human's first message IS the first turn, verbatim. Its image carries the toolchain the sandbox used to (bun, Go, git, gh, Playwright + a real browser), and Playwright runs in-pod on stdio as a second `mcpServers` entry rather than being proxied. Exits explicitly — a single-shot process that merely sets `process.exitCode` can hang forever with the right code and no exit. |
| `proto/` | buf-managed `.proto` schema (lint + breaking-change CI + generate/drift check): `CoreService` (core's gRPC server — agent/wrapper/provisioner-facing), `ProvisionerService` (the provisioner's gRPC server), `DashboardService` (ConnectRPC, dashboard ↔ core), `AgentService` (ConnectRPC, members and clients ↔ core, `adr/0066`) and `ExecutorService` (thot-executor, called by the `kubectl` shim). Generates Go (`proto/gen/go`) and TS (`dashboard/src/gen`, `protoc-gen-es`; the islands import it too). The worker has no generated code: it speaks HTTP/JSON to its sidecar. |
| `db/migrations/` | Shared Postgres schema (`agentfleetdb`, Pigsty-managed) — sole source of truth, applied via golang-migrate by the dedicated `migration` image on a `PreSync` hook ([`adr/0030`](adr/0030-single-source-schema-via-golang-migrate.md)). The files are the schema; §6 lists the tables. |
| `dashboard/` | React + Vite + TypeScript + Tailwind/DaisyUI SPA — the fleet's **primary** surface, rewritten as a console in [`adr/0042`](adr/0042-console-rewrite.md). Five full-width nav views (**sessions**, **schedules** — `?view=audits` still resolves —, **files**, **observability**, **settings**: repos' `active` toggle, context packs, definition grants), plus the session detail that replaces the list rather than sitting beside it — the 320px sidebar is gone. The list *is* the fleet overview: a live census, and a blocked session's pending decision rendered **and answerable inline** (an `Edit` as a real diff with allow/deny) off the same per-active-session transcript fetch the todo bars already used — no extra RPC. URL state is `?view=`/`?session=`; the pre-rename `?task=` is still read so links shared before v3.0.0 keep resolving. The detail view is feed · **decision dock** · composer, beside a fixed 266px panel column ([`adr/0043`](adr/0043-one-decision-surface.md) deleted the decision-spine rail: a pending decision now renders in the pinned dock and *only* there, on both form factors, via the shared `DecisionDock`); the feed ranks the twelve entry kinds into five visually distinct tiers (`SessionFeed`), gated by one three-way DENSITY control whose third mode deliberately differs between desktop and mobile (`feedVisibility`). **Mobile is designed, not ported** (`Agent Fleet Console Mobile.dc.html`): persistent top bar + bottom tab bar, bucket filter chips, ~44px targets, the blocking decision **docked above the composer**, and the panels as a bottom sheet — sharing `SessionFeed`/`SessionPanels`/`DecisionInline`/`DecisionDock`, plus the `bucketSessions` partition and the `sessionLabel` fallback, with desktop so the two can't drift. Two themes (`herd` dark default, `herd-light`) from the design tokens, applied pre-paint from `index.html`. **Session creation** (`NewSessionDialog.tsx`): repo alone is enough, title and first message are both optional, and an optional message is sent as a real `PostMessage` after `CreateSession` returns — which is what boots the pod (§4 below). Live transcript via a Connect server-streaming RPC; Warm/Interrupt/Kill/**Archive**, a mode picker showing `default`/`plan`/`acceptEdits`/`auto` with the real active mode highlighted, `AskUserQuestion`/`PermissionCard`/`PlanCard` answer forms (`adr/0018`/`0029`), a Loki log drawer, and the **proposals** gate on the schedules view — `ListProposals`/`OpenFromProposal`/`DismissProposal`, the human approval in front of every machine-initiated run. **observability** ([`adr/0047`](adr/0047-metrics-scoped-to-the-hubs.md)) is a live fleet topology plus a PromQL explorer: cells are the two hubs and one per live worker pod, coloured by status, and clicking one opens that session — the thing Grafana can't do, which is why the view is deliberately thin next to it and links to it. Hand-laid-out inline SVG, no `d3`/`cytoscape`: two fixed hubs and at most `MAX_IN_FLIGHT_TASKS` cells is a bounded shape whose positions are a loop. Talks to `core` via a generated `@connectrpc/connect-web` client. Built into and served by `core`'s binary — not deployed on its own. |
| `k8s/` | This repo's own deploy manifests: `core.yaml` (Helm values for `common-app-chart`, zero RBAC) and `provisioner/` (a standalone plain-manifest directory — `Deployment`/`Service`/`ServiceAccount`/`Role`/`InfisicalSecret`/`NetworkPolicy` ([`adr/0069`](adr/0069-component-auth-and-network-fences.md) §3)/`PersistentVolumeClaim`, plus the definition pods' egress `CiliumNetworkPolicy` (`definition-egress.yaml`, `adr/0065`) — since it needs RBAC `common-app-chart` can't express). Both referenced from `infra-bootstrap`'s `gitops/` (see §9). |
| `executor/` | Go — the only process in the fleet holding cluster RBAC ([`adr/0037`](adr/0037-thot-is-a-worker-task.md)). One RPC, `Exec(argv)`, run on behalf of pods that hold no credentials at all. Deployed from infra-bootstrap's `gitops/platform/thot/` as `thot-executor`, with the ClusterRole `adr/0032` reviewed. Reads are validated against a verb allowlist (nothing else checks them); mutations are a dumb pipe, because a human already approved that exact argv through `canUseTool`. |

## 2. End-to-end flow

```mermaid
sequenceDiagram
    participant Dash as Dashboard (primary interaction surface)
    participant D as Discord (secondary/notification)
    participant C as core (CoreService gRPC, DashboardService, reconcile loop)
    participant PG as Postgres (sessions + transcript)
    participant P as provisioner (ProvisionerService gRPC, git, k8s)
    participant SC as sidecar (in worker pod)
    participant W as worker (TS wrapper, in worker pod)
    participant Ag as Agent SDK session (streaming-input)

    Dash->>C: CreateSession(repo, title) — a ROW ONLY. No pod, no volume, no directory.
    C->>PG: INSERT INTO sessions

    Note over Dash,C: nothing has been provisioned. A session with no message is a valid resting state.

    Dash->>C: PostMessage(sessionId, text) — THIS is what boots a pod
    C->>PG: ReserveSlot(sessionId, MAX_IN_FLIGHT_TASKS) — advisory-locked cap, then mint a fresh lease
    C->>P: CreateWorkerPod(sessionId, repo, repoUrl, baseBranch, leaseId, resumeSessionId) [gRPC]
    P->>P: EnsureRepoCloned (clone cache, gc.auto=0, App token) + seed claude-home and /genome + mint a vault token
    P->>SC: client-go creates the session's PVC, then a batch/v1.Job: clone → sidecar → worker
    C->>PG: Append the message to transcript — AFTER the warm, never before
    P-->>C: ReportPodEvents stream (created→scheduled→running→...→[fast-path Failed], adr/0024) [gRPC]
    C->>PG: SetPodPhase — pod_phase drives concurrency/idle/stop checks now, not status (no journal write, adr/0055)

    W->>Ag: query() streaming-input, permissionMode from the session row ("auto" by default), resume: RESUME_SESSION_ID if set
    Ag->>SC: mcp tool call (local MCP, e.g. send_message)
    SC->>C: SendMessage [gRPC]
    C->>PG: Append to transcript (activityTrackingStore bumps last_active_at)

    Dash->>C: PostMessage(sessionId, text) — auto-warms an idle session first if needed
    C->>PG: Append (from=human, type=discussion)
    C-->>SC: StreamHumanMessages [gRPC server-stream]
    SC-->>W: GET /human-messages (SSE)
    W->>Ag: streamInput(reply text)

    Ag->>SC: tool call the current permission mode would prompt for
    SC->>C: (via canUseTool, blocked) Append transcript entry, type=permission_request
    C-->>Dash: PermissionCard/PlanCard renders the pending request
    C-->>D: NotifyBlocked — a one-line summary and a link, never the entry text
    Dash->>C: RespondToPermission(sessionId, seq, decisionJson)
    C->>PG: AppendReply(type=permission_response, reply_to_seq=seq)
    C-->>SC: StreamHumanMessages delivers the response
    SC-->>W: SSE event, type=permission_response
    W->>Ag: resolves the matching canUseTool Promise — allow/deny

    Ag->>Ag: write code, tests, docs; commit, push, gh pr create — all via Bash, in-session
    Note over Ag: the session does NOT end here — it idles, waiting for the next message
    Dash->>C: StopSession(sessionId) — a human decides it is finished
    C->>P: TearDownSession(sessionId, WORKER) [gRPC]
    P->>P: delete Job + unexpose; the session's PVC and SDK state are UNTOUCHED (still resumable)
```

Re-opening a session later doesn't restart this diagram from the top: `Warm`
(or the auto-warm inside `PostMessage`) calls the exact same `CreateWorkerPod`,
with `resumeSessionId` set from the session's saved `agent_session_id` — same
volume (still bound, and its node affinity brings the pod back to the same
node), same Claude conversation (resumed via `CLAUDE_CONFIG_DIR` + `resume:`),
new pod. `Warm` rejects with `CodeResourceExhausted` if the fleet is already at
`MAX_IN_FLIGHT_TASKS` live pods.

**The ordering is load-bearing: warm THEN append.** `resumeFromSeq` is computed
from `LatestSeq` at provisioning time, so a message appended before the pod
exists lands below the new pod's cursor and is never delivered — the pod boots
and sits there with nothing to do.

The dashboard's Stop button (or `AgentService.Stop`) posts a cooperative
`abort` transcript entry; `query.interrupt()` — the SDK's own graceful stop
primitive, not a process kill — plus `abortController.abort()` as a
backstop for the case where the session is idle and there's nothing active
to interrupt (`docs/adr/0021` point 3). If the worker doesn't honor it
within the stop-grace window, `core`'s session loop force-tears the pod down
the same way. Either way the session stays idle and **resumable via `Warm`** —
its volume and SDK state are untouched. The same force-teardown, without a
human asking, fires from a second sweep after `IDLE_TIMEOUT_MS` of no real
transcript activity.

Stop-grace is the only thing that can kill a runaway agent, which is why it
survives while so much else was deleted: a runaway keeps appending transcript
entries, so `last_active_at` keeps moving and the idle sweep never fires for
it.

Every `AskUserQuestion`/permission request carries the transcript's own
append-order sequence number; the human's reply threads back via a
`reply_to_seq` column (`transcript`), so a second question or tool-call
prompt posted before the first is answered can't have its reply
misattributed — a real gap under concurrent/rapid questions or parallel
tool calls, not a hypothetical one. Questions and permissions are answered in
the console, or through `AgentService` for a definition session; Discord only
says that a session is waiting (`core/internal/discord/session.go`).

During implementation the agent builds, tests, lints and installs with plain
`Bash`, in its own pod. There is no second pod and no `run_command`
(`adr/0048` §6).

That whole apparatus — a per-task e2e pod, `execmcp`, a `ServiceEndpoint`
roster, a per-task NetworkPolicy, an approval-gated `start_cmd` override and
three repair ADRs — existed so that **one tool could skip a permission
prompt**. `run_command` was un-prompted because the sandbox held no fleet
credentials and only that task's worktree; `Bash` stayed gated because the
worker pod held both. Collapsing the two pods removes that asymmetry, so the
un-prompted set has to be stated rather than implied: it is
`permissions.allow` in `fleet-shared/settings.json`, the same file and syntax
a CLI user edits. Builds and tests are on it; `git push`, `gh pr create` and
anything outward-facing are deliberately not, because approving those IS the
review. Keeping them prompted is an active rule, not an omission — a target
repo's own `permissions.allow` merges in alongside this one, so the outward-
facing set is held down by `FLEET_ASK_RULES` in `worker/src/session.ts`
(`docs/adr/0052`, moved there from this file by `adr/0049`'s original
`permissions.ask` block). Those rules outrank every permission mode, on every
session. What an ask *means* is decided one layer down, in `canUseTool`: in
`auto` it is answered there for everything but `rm`/`sudo` (`adr/0053`).

What survives fleet-side is the two things needing cluster RBAC the session
pod does not have, both routed agent → sidecar → `core` → `provisioner`:

- **`expose(port)` / `unexpose()`** — a `Service` and a Traefik `IngressRoute`
  publishing the agent's own server at `<shortId>-e2e.bnei.dev` (one label
  under `E2E_HOST`, `provisioner/internal/k8s/names.go`), behind authentik
  forwardAuth. The agent
  starts the server itself and asks for a URL; the fleet does not know how to
  start anything. Idempotent, because a warm replaces the pod while the
  hostname must not change.
- **`request_service("postgres"|"redis")`** — provisions or reuses a shared
  instance, keyed by repo. The one capability that could not move to the
  agent, because everything else the recipe system did was the fleet reading
  the repo on the agent's behalf.

Playwright runs in-pod on stdio, declared as a second `mcpServers` entry in
the worker's `query()` call, rather than being proxied through four hops from
an embedded tool-list snapshot. That deletes the entire class of failure
`adr/0044` documents: no snapshot to drift from the installed version, no
registration racing a pod that is still starting, and no hop to swallow an
error into an empty result.

**MCP is local, full stop** (`docs/adr/0048` §6, superseding `adr/0045` and
restoring `adr/0020` point 6 unqualified). The agent reaches only its own
pod's sidecar over `localhost`, and there is nothing else to reach: the
sandbox the sidecar used to dial is merged into the worker pod. gRPC remains
the only protocol *between fleet components*, and the provisioner does not
speak MCP at all (pinned by a `buildguard` import test).

The per-sandbox NetworkPolicy that used to fence that cross-pod dial went with
it. It was a real structural control, and worth remembering as one — but the
thing it protected no longer exists, and a policy guarding nothing is a policy
nobody maintains.

## 2b. Who may call what (authentication)

The console and `CoreService` are two mechanisms that are not
interchangeable — see `adr/0056` and `adr/0057`. `AgentService` adds two
principals on top (`adr/0066` §1–§4): its own per-method table in
`core/internal/agentsvc/authz.go` decides what each may call, and fails
closed.

| Surface | Port | Who | Credential |
|---|---|---|---|
| SPA + `DashboardService` | 8080 | a human in a browser | authentik OIDC → `__Host-fleet_session` cookie, signed by core; `platform-admins` only |
| `AgentService` (members) | 8080 | a definition's audience member, from the console or an island (`adr/0068`) | the same cookie; any group in `KnownGroups` (`core/internal/definitions/load.go`) may log in, and reaches `AgentService` only |
| `AgentService` (clients) | 9091 | a registered client (`definitions/clients/`) | a bearer per registration, compared in constant time; in-process clients (the bridge) have none |
| `CoreService` | 9090 | a session's sidecar | that session's `lease_id`, verified against the `sessions` table |
| `CoreService` | 9090 | the provisioner | `FLEET_PROVISIONER_TOKEN` (it holds no lease — it exists before any pod does) |
| `ProvisionerService` | 9090 (provisioner) | core | `authorization: Bearer $FLEET_CORE_PROVISIONER_TOKEN`; the provisioner refuses to start without it (`adr/0069`) |
| `ExecutorService` | 9090 (`thot-executor`) | a `cluster_access` session's kubectl shim | `authorization: Bearer $THOT_AUTH_TOKEN`; the executor refuses to start without it (`adr/0069`) |
| `<id>-e2e.bnei.dev` previews | — | a human in a browser | authentik **forwardAuth** at Traefik |

**Why the console does not use forwardAuth**, which is the obvious answer and
was the original plan (#209, infra ADR-0039): a middleware gates the *ingress*,
and a worker pod POSTing to `agent-fleet-core.agent-fleet.svc.cluster.local:8080`
never reaches it. That is #200. A check inside the process applies regardless of
network position. The previews go the other way for the mirror reason — they
route to a session's own dev-server pod, so there is no fleet code in the
request path to terminate OIDC with, and their hostnames are minted at runtime
so no per-host provider can be declared.

**`fleet.bnei.dev` has exactly one lock.** `basic-admin-auth` was removed once a
real login was proven (infra ADR-0041's amendment). The IngressRoute matches on
host with **no path constraint**, so every path core serves on 8080 is public
unless the in-app gate refuses it:

| Path | Gated? |
|---|---|
| `/` (SPA), `/agentfleet.v1.DashboardService/*` | yes, `platform-admins` |
| `/agentfleet.v1.AgentService/*` | yes, any audience group; CORS only for `embed.origins` |
| `/healthz`, `/auth/{login,callback,logout,me,done}` (exact) | no — inert |
| `/islands/*` | no — static bundles, no data (`adr/0068`) |
| `/webhook/alertmanager` | no — its own bearer token, refuses when unset |

**A new exempt path is a new public endpoint.** The gate wraps the whole mux
with an explicit exempt list precisely so a forgotten route fails closed rather
than defaulting open.

`/metrics` is **not on 8080 at all** — it has its own listener on 9093, which no
IngressRoute reaches ([`adr/0059`](adr/0059-metrics-off-the-gated-port.md)). It
used to sit on the gated mux, where the ServiceMonitor's cookieless scrape was
refused with a 401 like any other anonymous caller and every core target went
down. Exempting it was the obvious fix and the wrong one: with no path constraint
on the route, that publishes every target repo name and RPC method.

**Who may even connect (network fences, [`adr/0069`](adr/0069-component-auth-and-network-fences.md) §3).**
Ingress-only NetworkPolicies select core and the provisioner. Worker pods,
shared instances and `expose-*` Services are not selected yet and stay
reachable from any namespace; §3 defers them to the ADR-0064 work. Definition
pods have an **egress** lock instead (`k8s/provisioner/definition-egress.yaml`,
[`adr/0065`](adr/0065-agent-vault-and-the-definition-egress-lock.md) §5): the
Agent Vault proxy `:14322`, core `:9090` and DNS for those two names only. The
credentials above remain the authentication. The fences only cut down who can
try.

| Pod:port | Admitted from |
|---|---|
| core `:8080` | `traefik` namespace; Alertmanager and Grafana in `monitoring` (both post `/webhook/alertmanager` to the ClusterIP directly, not through the ingress) |
| core `:9090` | worker pods (`agent-fleet.dev/component=worker`) and the provisioner, same namespace only |
| core `:9091` | namespaces labelled `fleet.bnei.dev/client` — never `agent-fleet` itself |
| core `:9093` | Prometheus |
| provisioner `:9090` | core |
| provisioner `:8080` | Prometheus |

core's policy is an `extraManifests` entry in `k8s/core.yaml`; the
provisioner's is `k8s/provisioner/networkpolicy.yaml`. Each lives with the
Application that owns its pod. `provisioner/internal/k8s/networkpolicy_test.go`
pins the in-repo labels and ports (worker, shared instance, provisioner,
core's service ports and `CORE_*_PORT`) and that both policies are ingress only.
The external callers' labels and core's release name (infra-bootstrap's
`registry.yaml`) are only asserted. A `hostNetwork` pod on the target's node
gets past every fence, and so may NodePort/LB traffic kube-proxy SNATs on that
node; §3 names who can create either today. The `:8080` fence refuses a worker
pod's *direct* connection only: through `fleet.bnei.dev` it arrives as Traefik,
so the OIDC gate stays the boundary there.

**Failure behaviour.** Unset OIDC config makes core refuse to start, unlike
every other optional secret it reads — the Infisical operator renders stale or
empty Secrets often enough that "off when unset" would come up wide open and
looking healthy. `FLEET_AUTH_DISABLED=1` is the explicit local-stack opt-out.
The cost, accepted knowingly: core has no local admin, so an authentik → Pigsty
→ Patroni outage means no console at all. Recovery is `FLEET_AUTH_DISABLED=1`
plus a redeploy, or `kubectl port-forward`.

**What is deliberately not done.** core never reads `X-authentik-*` headers: a
pod-network caller can forge one, turning an authorization gap into an
impersonation gap. And there is still no `users` table — `/auth/me` returns the
verified claim (email, groups) for the console's own chrome, not a profile.

## 3. Permission model

Supersedes the old "Planning-phase guardrails" — there's no fleet-imposed
phase left to guard. `canUseTool` reproduces the Agent SDK's own CLI-parity
permission tiers instead of a second, fleet-specific gate on top of them
(`adr/0029`).

- **`canUseTool` does zero tool classification of its own.** The old
  `MUTATING_BASH_RE` heuristic and the hardcoded `Write`/`Edit` check are
  both deleted, not widened. The SDK's real `permissionMode`
  (`'default' | 'plan' | 'acceptEdits' | 'auto' | 'delegate' | 'dontAsk'`)
  already decides *when* `canUseTool` gets invoked —
  `acceptEdits` skips it for file
  edits, `auto` (SDK 0.3.233) has a model classifier answer the ordinary
  prompts instead of a human, `plan` blocks mutation without invoking it,
  `default` invokes it
  for anything not auto-safe (`Read`/`Glob`/`Grep` stay in `allowedTools`,
  so the SDK never even calls the callback for those; `WebSearch`/`WebFetch`
  are disallowed, `docs/adr/0082` §3). A tainted session (`docs/adr/0082` §4:
  an untrusted entry, or an outside read the worker marked through core's
  `Taint`) may only `Read`/`Glob`/`Grep`: `canUseTool` and a `PreToolUse` hook
  deny the rest, whatever the mode, `tools.allow` or a grant says, and core
  refuses its `PublishEvent`, `PromptSession`, `Expose` and `RequestService`. `canUseTool`'s only remaining job: post a `permission_request`
  transcript entry and block on the matching `permission_response` —
  generalizing `ExitPlanMode`'s own original ask-and-block-and-resolve
  pattern from one hardcoded case to a `Map<seq, resolver>` so parallel
  tool calls in one assistant turn each get their own correlated prompt.
  Two exceptions, and they are answers rather than classifications
  (`adr/0053`): the fleet's own MCP tools are allowed outright — otherwise the
  agent needs permission before it can *ask* for permission, which is what
  shipped — and in `auto` everything but a `Bash` running `rm`/`sudo` is too.
  Neither can be expressed as a rule: 0.3.233 asks for every non-read-only MCP
  tool in `plan` mode, and again under an org `effectiveMaxPermission` ceiling,
  both above the allow-rule lookup.
- **Sessions start in `"auto"` mode** (`docs/adr/0061`; it was `"default"`,
  CLI parity, until then), not a forced `"plan"` gate. `plan` is still fully
  available, just as one selectable mode via the dashboard's mode picker
  (`default`/`plan`/`acceptEdits`/`auto`, highlighting
  the real active mode via the `sessions.permission_mode` column) rather than
  a mandatory starting state. Every switch is live — `bypassPermissions`, the
  one mode the SDK fixed at launch, is deleted (`docs/adr/0053`). A
  refused switch is reported to the human and the column is written back to
  the live mode — never swallowed into a log line that leaves the badge
  claiming a mode the SDK is not in.
- **`Approve` no longer exists** — proto RPC, Go handler, Discord
  `/approve` command and handler are all deleted (including explicit
  stale-command pruning: `discordgo.ApplicationCommandCreate` only upserts
  by name, it never deregisters a dropped command on its own).
  `SetPermissionMode` (widened to accept `default`/`plan` alongside the
  pre-existing `acceptEdits`, `adr/0027`, plus `auto` in `adr/0052`) is the only mode lever now — including for plan approval,
  which is a `SetPermissionMode` followed by a `RespondToPermission` rather
  than a mode change of its own: the agent's next turn starts the moment
  `canUseTool` resolves, so a mode set after the response lands too late.
  A new `RespondToPermission` RPC — `AnswerQuestion`'s
  sibling, same `AppendReply`-by-seq shape — answers individual
  `canUseTool` prompts. `docs/adr/0005`'s "never inferred from silence or
  sentiment" rule is unchanged: a permission decision is still a real,
  structured RPC call, never inferred.
- **Complexity-gated interview/doubt, no `/task` knob.** The agent decides
  for itself, per task, whether `architecture-interview` and/or
  `doubt-driven-development` apply, using each skill's own "when to use"
  criteria. Mohammad can still interject live in the thread at any point
  ("skip the interview" / "run doubt on this") — narrative discussion is
  fed straight into the running session via `streamInput()`
  (`docs/adr/0021`), reaching the model live instead of waiting for it to
  poll — see [`adr/0017`](adr/0017-single-session-planning-pipeline.md).
- **No round cap, no phase boundary — the agent paces itself.** There is no
  fleet-composed prompt: the human's first message is the first turn, and the
  agent decides when to explore, plan out loud and implement. Nothing marks a
  session complete — no `PR_READY:` line, no PR check. A session idles between
  turns until the next message, a Stop, or the idle timeout.
- **`AskUserQuestion`, correlated by sequence.** A real `AskUserQuestion` MCP
  tool (proxied through the sidecar → `core`; the SDK's built-in one is
  disallowed) posts a `question`-type `transcript` entry and waits for the
  matching `answer`, threaded via `reply_to_seq` so a second question posted
  before the first is answered can't have its reply misattributed. The same
  correlation covers permission requests. Answered in the console or through
  `AgentService`; Discord only says that a session is waiting.
- **Definition sessions decide differently** ([`adr/0076`](adr/0076-definition-sessions-run-in-auto.md),
  [`adr/0067`](adr/0067-shared-and-private-sessions.md) §4). Their `auto` is the
  fleet's: the worker runs the SDK in `default`, so every call that is not
  auto-safe reaches `canUseTool`, where `definitionDecision`
  (`worker/src/session.ts`) runs a call `tools.allow` matches, asks for the
  rest of a fixed tool ceiling, and denies anything outside it. An answer can
  grant the exact rule for the session or (admins) the definition
  (`core/internal/grants`, `session_grants`/`definition_grants`).
- **Turn/time limits are opt-in, not default.** `MAX_TURNS` aside, nothing
  else is bounded unless explicitly set — see
  [`adr/0008`](adr/0008-unbounded-guardrail-defaults.md). There's no round
  concept left to cap.
- **No cost cap.** Claude Code authenticates via `CLAUDE_CODE_OAUTH_TOKEN`
  (subscription), not a metered API key, so `total_cost_usd` in SDK
  results is notional, not a real charge.
- Every SDK message (not just the final result) is logged
  (`worker/src/session.ts`'s `logSdkMessage`), and every assistant text
  block is relayed into the transcript (`sidecar.pushMessage`) — added
  after a real incident where a missing `allowedTools` entry silently
  denied every MCP tool call with zero visible signal until cost/turn-count
  was inspected after the fact.

## 4. Current features (the golden path, working today)

A pointer list: each line names the code that is the detail and the ADR that
is the reason.

- **Sessions start from a row and boot on the first message** (`adr/0048` §1).
  `CreateSession` writes a row; the first `PostMessage` (or `Warm`,
  `OpenFromProposal`, `PromptSession`, `AgentService.Open`) provisions through
  `WarmIfIdle` (`core/internal/dashboard/server.go`). Warm-then-append, never
  the reverse (§2). `title`/`description` are labels the agent never reads; the
  instruction is the first transcript entry.
- **Pod lifecycle is `pod_phase` alone**, reconciled every 60s by
  `sessions.Loop` (`core/internal/sessions/reconcile.go`) with the stop-grace,
  startup-stall, idle-timeout and retention sweeps. Live pods are capped by
  `MAX_IN_FLIGHT_TASKS` (default 5) in `ReserveSlot`, under an advisory lock;
  a session that cannot get a slot is refused, nothing queues. Crash
  reporting, not crash recovery: both terminal Job phases are reported, and
  warming again is a human's decision.
- **A session's disk is its own and survives teardown** (§5): a per-session
  PVC holding a real clone, plus per-session SDK state; only the retention GC
  (`SweepSession`) reclaims them. The agent names its own branch.
- **One continuous Agent SDK session per warm**, resumed across pods with
  `RESUME_SESSION_ID` (`worker/src/session.ts`, `adr/0029`). Permissions: §3.
- **Discord is outbound-plus-a-link** (`core/internal/discord/session.go`,
  `adr/0048`): it says that a session needs a human, never what. No slash
  commands, threads or relayed replies — Discord has no authorization model,
  so a button there would let anyone in the channel approve a `Bash`.
- **Proposals hold untrusted machine-initiated work**: alerts
  (`core/internal/alertwebhook`), schedules (`core/internal/schedules`) and
  bridge events from untrusted actors (`source='event'`) file deduped
  proposals; a human opens one (`OpenFromProposal`).
- **Core is on the event bridge** (infra-bootstrap ADR-0060; `adr/0071`,
  `adr/0078`). One dispatcher (`core/internal/dispatch`) consumes
  `dev.bnei.github.`/`alert.`/`agent.` events and routes each to the
  definitions that own or subscribe to it, through in-process
  `AgentService.Open` as the `bridge` client. A trusted human's event wakes
  a thread; anything else becomes a proposal. Delivery and retry are the
  bridge's: core acks or nacks, and holds no queue. A definition granted
  `publish_event` hands work on through `CoreService.PublishEvent`
  (`dispatch/publish.go`; depth, per-session budget and per-thread wake
  caps). `core/internal/announce` publishes `session.waiting`/`resumed`
  for clients with `announceSessions`. `definitions/clients/bridge.yaml`
  lists no definition yet, so no event opens anything.
- **Repos come from GitHub** ([`adr/0074`](adr/0074-fleet-repos-come-from-github.md),
  amended by `adr/0075` §4): core projects the GitHub App `ukubi-fleet`'s
  allowlisted installations into `repos` (`core/internal/repos/sync.go`, via
  the provisioner's `ListGitHubRepos`); overrides come from infra-bootstrap's
  `gitops/bootstrap/fleet-repo-overrides.yaml`; the console toggles `active`.
- **Credentials: GitHub App + Agent Vault, no secret in a pod**
  ([`adr/0075`](adr/0075-github-app-identity-and-vault-credentials.md),
  [`adr/0065`](adr/0065-agent-vault-and-the-definition-egress-lock.md)). The
  provisioner holds the App key and mints installation tokens
  (`provisioner/internal/github`), and is the vault's only client
  (`provisioner/internal/vault`). Workers get placeholders; the vault injects
  the real token on the wire — for repo pods through the sidecar's egress
  router (`sidecar/internal/egress`), for definition pods through an egress
  lock (`k8s/provisioner/definition-egress.yaml`). Repo pods commit as
  `FLEET_GIT_AUTHOR_*` (`ukubi`); definition pods as their file's
  `git.identity` (`adr/0064`). No token carries `workflows`.
- **Defined agents** ([`adr/0062`](adr/0062-defined-agents.md),
  [`0064`](adr/0064-definition-pod-profile.md), [`0065`](adr/0065-agent-vault-and-the-definition-egress-lock.md),
  [`0076`](adr/0076-definition-sessions-run-in-auto.md)): reviewed
  `definitions/*.yaml`, validated by `core/internal/definitions` (CI:
  `repo_test.go`), read by the provisioner at its release tag, synced into
  `agent_definitions`, pinned per session by `(name, sha)`, run in a narrower
  pod. Pool: `MAX_DEFINITION_SESSIONS` inside the global cap.
- **AgentService** ([`adr/0066`](adr/0066-agent-service.md),
  [`0067`](adr/0067-shared-and-private-sessions.md), `core/internal/agentsvc`):
  members on `:8080` and registered clients on `:9091` drive definition
  sessions as threads — shared or private, with recorded authorship and
  taint. `Invoke`/`WaitForResult` are in the proto but not built
  ([`adr/0070`](adr/0070-headless-invoke.md)). Pool: `MAX_EXTERNAL_SESSIONS`.
- **Islands** ([`adr/0068`](adr/0068-islands.md), `islands/`,
  `core/internal/islands`): `<fleet-agent>`, a Svelte custom element served
  at `/islands/`, puts a definition's threads on other `bnei.dev` pages.
- **Grants** ([`adr/0076`](adr/0076-definition-sessions-run-in-auto.md) §3,
  `core/internal/grants`): a permission answer on a definition session can
  remember its rule for the session or, by an admin, the definition.
- **Context packs** ([`adr/0063`](adr/0063-context-packs.md),
  `core/internal/packs`, `packs/`): live packs edited in the console and
  reviewed packs from `packs/`, rendered by core on every warm into the
  session's read-only `/genome`; the worker reports what loaded
  (`context_loaded`).
- **`expose(port)` and `request_service(kind)`** (§2): the two capabilities
  that need cluster RBAC the session pod lacks.
- **Fleet-wide shared files** on Garage S3 (`docs/adr/0031`): core mints
  presigned URLs only; agents use the `*_shared_file*` MCP tools and `curl`.
- **The journal is agent knowledge** (`adr/0055`): `journal_write` /
  `journal_search`; nothing writes to it automatically.
- **Peer sessions**: `prompt_agent` through core (`adr/0041`), plus Claude
  Code's own cross-session channel, closed for definition pods (`adr/0060`,
  `adr/0064`).

## 5. Deployment shape

**Storage is split by access pattern, not by uniformity** (`adr/0048` §4).
It used to be one `ReadWriteMany` PVC holding everything — working trees,
`node_modules`, SDK state. Measured, that volume runs at 10 MB/s, where a cold
`bun install` could not finish in three minutes; the same install takes 2.4
seconds on node-local disk. So a session's pod mounts four things:

```
/workspace          per-session local-path PVC, subPath "tree"   node-local, pinned
/cache              same PVC, subPath "cache"                    node-local, warm across warms
/repo-cache         shared RWX, subPath "repos", READ-ONLY       the clone cache
/claude-home        shared RWX, subPath "claude-home/<id>"       SDK resume state
```

The pinning is the feature: `WaitForFirstConsumer` binds the session's volume
to whichever node its first pod lands on, and the volume's node affinity then
forces every later warm back to that node — so a resumed session finds its tree
and its warm cache already there, with no affinity rules to write and no
re-clone. The cost is accepted: a drained node strands its sessions until it
returns, and git is the durable copy.

`/workspace` being the same literal path for every session is load-bearing and
newly dangerous. The SDK derives its project-state directory from `cwd`, so
every session now encodes to the same `projects/-workspace/`; the per-session
`claude-home` mount is the only thing keeping them apart.

Only the provisioner (clone cache, `claude-home` seeding) and session pods
touch the shared volume — `core` holds zero PVC access, matching its zero-RBAC
design.

**A session is single-shot, one pod, spawned on demand — not persistent, not
one Deployment per repo, and not two pods.** The provisioner builds a
`batch/v1.Job` directly via `client-go` (`provisioner/internal/k8s/pod.go`,
`adr/0022`). The pod holds a clone init container, the sidecar as a native
sidecar, and the worker:
- `clone` init container, FIRST and ordered so: a plain init container runs to
  completion before a native sidecar starts, so the working tree exists before
  anything reads it. It is the only place both `/workspace` and the read-only
  `/repo-cache` are mounted, which is why the clone happens here rather than in
  the provisioner — and it runs on the node that owns the volume. Re-running on
  a warm is a no-op: an existing repository is left exactly as it is,
  uncommitted work included.
- `sidecar` container: the Go image, as a **native sidecar** (init container
  with `RestartPolicy: Always` plus an HTTP `/readyz` StartupProbe), so kubelet
  blocks the worker until the sidecar has a proven connection to core.
  `50m`–`250m` CPU / `64Mi`–`256Mi` memory.
- `worker` container: the TS/Bun image, `250m`–`2000m` CPU / `512Mi`–`2Gi`
  memory; its env is built from an explicit list in
  `provisioner/internal/k8s/pod.go`, never `envFrom`. **No pod holds a real
  secret** (`adr/0065`, `adr/0075` §7): no `LEASE_ID` in the worker (the
  sidecar has it), placeholder `CLAUDE_CODE_OAUTH_TOKEN`/`GH_TOKEN`, a CA
  bundle with the vault's root, and `HTTPS_PROXY` toward Agent Vault
  (`agent-vault.agent-vault`, infra-bootstrap ADR-0061) carrying a per-warm
  token the provisioner minted and revokes on every teardown path. Job
  `activeDeadlineSeconds` caps every pod at 12h; a re-warm resumes. A repo
  pod's proxy is the sidecar's egress router, so only the GitHub and
  Anthropic hosts touch the vault (its owner's `owner-<login>` vault) and the
  rest goes direct; egress is open. A definition session's pod (`adr/0064`)
  is narrower: it mounts only `/repo-cache/<repo>` (subPath `repos/<repo>`) in
  all three containers, clones from the cache's `origin/<base>` with no
  remote contact, skips the fleet-shared sync, sets `FLEET_DEFINITION` (the
  worker then refuses Claude Code's own cross-session channel), carries the
  `fleet.bnei.dev/definition` label, proxies straight to its definition's
  vault, sets `ndots:1`, and sits behind an egress lock. `limits.timeout` is
  its idle timeout, applied by core's idle sweep.
- Mounts are `subPath`-scoped, which is real isolation for the first time. It
  used to be impossible: a linked worktree's `.git` is an absolute-path gitlink
  back to `repos/<repo>/.git/worktrees/<taskId>`, so scoping the mount severed
  it and every git command answered "not a git repository" (found live,
  2026-08-05). A session's tree is a real clone now, so nothing points outside
  its own volume.
- `RestartPolicy: Never`, `BackoffLimit: 0` — a crashed session is not
  restarted by Kubernetes or by the Job. It surfaces as `pod_phase = CRASHED`
  with its reason, and warming it again is a human's decision.

**`core` needs zero cluster RBAC**, so it deploys via this repo's own
`k8s/core.yaml` through `common-app-chart` (two-source ArgoCD Application,
chart from `infra-bootstrap`, values from here) — no standalone-Application
escape hatch needed. `core` is this repo's first app with a real listening
`service.port` (`8080`, publicly reachable — the dashboard/`/healthz`) and
`ClusterIP`-only ports: `CORE_GRPC_PORT` (`9090`, `CoreService`),
`CORE_CLIENT_PORT` (`9091`, `AgentService` for clients) and
`CORE_METRICS_PORT` (`9093`). Its HTTP ingress has no middleware: the OIDC
gate inside core is the one lock (§2b, `adr/0056`).

**`provisioner` needs real RBAC**, so — unlike `core` — it is **not**
deployed through `common-app-chart`. As of this redesign its manifests
moved *into this repo* (`k8s/provisioner/`: `Deployment`, `Service`,
`ServiceAccount`, namespaced `Role`, `InfisicalSecret`, `NetworkPolicy`
(`adr/0069` §3), `PersistentVolumeClaim`), registered in `infra-bootstrap` as a standalone
plain-manifest ArgoCD Application (`gitops/bootstrap/
provisioner-application.yaml`) pointed at this repo instead of
`infra-bootstrap/gitops/platform/e2e-provisioner/` (the old location, now
deleted) — see §9. Its namespaced `Role` (`k8s/provisioner/role.yaml`) is
the list of what it may touch; `batch/jobs` was once missing from it, a gap
the fake-clientset unit tests didn't catch and only a real `kind`-cluster
smoke test did.

### Deploy pipeline

`.github/workflows/docker.yml` is the pipeline; read it for the detail. On a
push to `main`:

1. `release` runs `release-it` (Conventional Commits → version, `CHANGELOG.md`,
   tag), gated on the worker and dashboard test jobs.
2. `build-push`, one job on the `build-runner` LXC (a self-hosted runner does
   one job at a time; infra-bootstrap ADR-0034), builds the six images
   (`$COMPONENTS`: `core`, `provisioner`, `sidecar`, `executor`, `worker`,
   `migration`) with `buildah` and pushes content-addressed `src-<hash>` tags
   to Zot at `registry.bnei.lan:5000` (see "Which build is running"). On a PR
   only changed components build, and nothing is pushed.
3. `deploy` pins those tags and `FLEET_VERSION` in `k8s/core.yaml` and
   `k8s/provisioner/*.yaml`, writes `k8s/images.env`, checks every pin
   landed (a sed that stops matching is a green no-op otherwise), and commits
   to `main` with the built-in token, which triggers no workflow.
4. ArgoCD syncs `core` (two-source chart Application) and `provisioner`
   (standalone plain-manifest Application). `db/migrations/` is applied by
   the `migration` image as a `PreSync` hook on `core`'s Application
   (`docs/adr/0030`); `core` embeds no schema.

`.github/workflows/go.yml` is the Go correctness gate on every push/PR:
`go vet`, `golangci-lint`, `go test -race`, `-tags=integration` against a
real Postgres, plus `buf lint`/`buf breaking`/a generate-drift check.

### Observability

Two stores, split by what produces the data — see
[`adr/0047`](adr/0047-metrics-scoped-to-the-hubs.md).

**Loki** (via Alloy) holds everything every component logs, and is the
*only* store for worker and sidecar telemetry. Worker pods are single-shot
Jobs that routinely start and exit between two 30s scrapes, so a Prometheus
counter on them would sample a random subset of sessions; Loki keeps every
line regardless of how briefly the pod lived. The SDK's per-session
turns/cost/input tokens are already structured fields on `session.ts`'s
`result` log line, queried with LogQL `unwrap`.

**Prometheus** scrapes `core` and the `provisioner` only — the two
long-lived components, each with a real Service:

| Component | `/metrics` | Scraped by |
|---|---|---|
| `core` | `:9093` — its own listener, **not** the gated 8080 | ServiceMonitor in `k8s/core.yaml`'s `extraManifests`, on the named `metrics` port |
| `provisioner` | `:8080` | `k8s/provisioner/servicemonitor.yaml` |

core's separate port is not tidiness: 8080 is wrapped in the console's OIDC gate
and its IngressRoute matches on host with no path constraint, so `/metrics` there
was refused for Prometheus and routable for the internet at once
([`adr/0059`](adr/0059-metrics-off-the-gated-port.md)). The provisioner has no
ingress in front of it and needs no such split.

A ServiceMonitor, **not** `prometheus.io/scrape` annotations: this cluster
runs kube-prometheus-stack (the Prometheus Operator) and `infra-bootstrap`'s
prometheus values declare no `additionalScrapeConfigs`, so there is no
`kubernetes-pods` job for an annotation to drive — annotations here are
inert while *looking* like configuration.

Eight metrics, each incremented somewhere, chosen for what neither Loki
(RPC access logs) nor kube-state-metrics/cAdvisor (pod counts, phases,
restarts, CPU, memory) already answers. `agentfleet_tasks_current{repo,
status}` is the load-bearing one — how many sessions are in each **live
state** right now, which is what makes "N sessions blocked for 30m"
alertable. Its name and its `status` label predate `adr/0048` and are
deliberately left alone: renaming a metric breaks every Grafana panel and
alert rule built on it, and `publishLiveStateGauge` in
`core/internal/sessions/reconcile.go` is the single writer, so the label's
real meaning is knowable from one place. No `session_id` label anywhere:
session ids are unbounded, and per-session detail is what the transcript and
Loki are for.

Surfaced in two places: a "Fleet metrics" row on the existing Grafana
dashboard (`k8s/core.yaml`), and the dashboard's own **observability** view,
which proxies PromQL through `core` (Prometheus has no IngressRoute, so the
browser cannot reach it directly) and renders the live topology from
`sessions.pod_phase` — so `core` still holds zero cluster RBAC.

### Which build is running

`core` and `provisioner` report the release from their `FLEET_VERSION` env,
which `docker.yml`'s deploy job sets alongside the image pins (`dev` without
it). Images are tagged `src-<hash of their inputs, the ISO week and a salt>`,
not by release. An image whose tag the registry already holds is reused, not
rebuilt, and only a run that deploys pushes (dispatch and stale re-runs build
without pushing), so a binary cannot know its release. `k8s/images.env`
records what deploy pinned. Accepted: the first release of each ISO week
rebuilds and rolls everything, and a `_test.go`/`.md` edit inside a
component's directory rebuilds it. `worker` and `sidecar` show their image tag, the
`src-…` the provisioner puts in the pod spec, which is the truth actually
deployed. The dashboard SPA
is compiled into `core`'s binary, so its version is `core`'s.

Two different questions, answered from two different places:

- **What will a new session boot?** `ProvisionerService.GetVersion` — the
  provisioner's own build plus its `WORKER_IMAGE`/`SIDECAR_IMAGE` defaults.
  Process-local constants, no Kubernetes call, so the topology's 5s poll can
  ask on every tick. Unreachable leaves the fields blank; it never empties the
  graph, same posture as `metrics_error`.
- **What is this session running?** `sessions.worker_image`/`sidecar_image`,
  written from `CreateWorkerPodResponse` at dispatch and then left alone. A
  session warmed before a fleet upgrade keeps running the older worker until
  its next warm, so reading today's defaults for it would be wrong during
  exactly the window someone is asking. Shown in the session panel on both
  form factors, and on that session's topology cell.

## 6. Data model

The schema is `db/migrations/` (golang-migrate, `adr/0030`); read the newest
files there. Tables: `sessions`, `transcript`, `proposals`, `repos`, `schedules`,
`knowledge_journal`, `agent_definitions`, `packs`/`pack_versions`/`pack_attachments`,
`reviewed_packs`, `client_requests`, `session_grants`, `definition_grants`,
`session_announcements`, `dispatch_decisions`, `published_events`.
Invariants the files only imply: `sessions` has no status, lease-renewal or
retry columns (`adr/0048` §2) and `pod_phase` is its only liveness signal;
`transcript` cascades from `sessions` and dedups on `(session_id,
idempotency_key)`; `knowledge_journal` is append-only and deliberately not
FK'd to `sessions`.

## 7. Environment variables

Each Go binary reads its environment once, in its `internal/config` package
(`FromProcess`; the executor's are `ExecutorFromProcess` and `ShimFromProcess`),
and an AST test (`TestEnvBan`) fails on any other read, apart from the
child-process pass-throughs and library reads each test names (git and kubectl
run with the process's environment; the provisioner's genome staging directory
is made under a path, never `$TMPDIR`). That file is the list, with defaults and
comments. Below, only what a config file
cannot say.

### `core/`

`core/internal/config/config.go`. `AGENTFLEET_DB_*` exist only here. Caps:
`MAX_IN_FLIGHT_TASKS` (live pods) ⊃ `MAX_DEFINITION_SESSIONS` ⊃
`MAX_EXTERNAL_SESSIONS` (members and clients); each pool must be smaller than
the one around it (0 turns an inner pool off). A set of counts that breaks that
logs a warning and falls back to all four defaults (5, 0, 0, 1), which turns the
inner pools off; core still starts.

**Validated once, at start.** `config.Load` reads every variable below and
checks each one that is *set* against its rule. Unset, or empty (the two cannot
be told apart), is the default; a secret key rendered empty is "unset", never a
problem. A bad value is handled by the key's class:

- **Optional (warns, uses the default):** every key with a default or that may
  be unset. The problem is recorded in `Config.Warnings` as
  `KEY=value: rule; using the default`, the field takes the default, and core
  starts. `main` logs one `WARN` line per problem (`invalid configuration value,
  using the default`, the problem in `problem`) as JSON at the default level, so
  `LOG_LEVEL=error` does not hide it (a refusal logs the warnings found with it
  first). This includes a zero or negative `*_MS` or `SESSION_RETENTION` (they
  fall back to the default and never reach a sweep), and `FLEET_AUTH_DISABLED`
  other than `1` or `0` (read as gate on). The `BRIDGE_*` URL, token URL and auth
  host are one setting: if any is set and invalid, each problem ends `; the
  bridge is off` and `BRIDGE_URL` is blanked, the bridge's not-configured mode.
- **Required (`(refuses)` in the table), only while the console gate is on**
  (`FLEET_AUTH_DISABLED` is not `1`; with it, nothing reads them and a bad value
  only warns): the keys without which nobody can sign in. A malformed
  `OIDC_ISSUER_URL` or `DASHBOARD_PUBLIC_URL` stops the start in `Load`: one error
  names every such variable and its rule, and `main` logs `invalid configuration`
  and exits 1, before the database is opened. A *missing* `OIDC_ISSUER_URL`,
  `DASHBOARD_PUBLIC_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` or
  `FLEET_SESSION_KEYS` stops it in `run()` (`requireOIDC` and `auth.New`, see
  Auth below), after the database is opened and naming the variable. `Load` of an
  empty environment succeeds: a missing value is not a problem in `Load`.
- **The pools' relation** is a warning too: on the values in use (an unparsed
  count is its default), a relation that fails logs `checkCapacity`'s message and
  falls back to all four defaults, which turns the inner pools off.

A value with `@` in it (URL userinfo) is never echoed, in a warning or a
refusal. `TestConfigDocumented` fails on a variable core reads that is not a row
here, and on a row nothing reads. The libraries' own variables (`AWS_*`, `PG*`,
the proxy variables) are theirs and not listed.

| Variable | Default | Rule when set | What it is |
|---|---|---|---|
| `CORE_PORT` | `8080` | port, 1-65535 | the console, `ConnectRPC`, `/auth/*`, the alert webhook (internet-routed, behind the OIDC gate) |
| `CORE_GRPC_PORT` | `9090` | port | `CoreService`, for the sidecar and the provisioner |
| `CORE_METRICS_PORT` | `9093` | port | `/metrics` on its own listener, no ingress (`adr/0059`) |
| `CORE_CLIENT_PORT` | `9091` | port | `AgentService` for registered clients (`adr/0066` §3) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`, any case, no offset | log level |
| `AGENTFLEET_DB_HOST` | `postgres.bnei.lan` | any | Postgres host; `AGENTFLEET_DB_*` are read here only |
| `AGENTFLEET_DB_PORT` | `5432` | port | Postgres port |
| `AGENTFLEET_DB_NAME` | `agentfleetdb` | any | database |
| `AGENTFLEET_DB_USER` | `dbuser_agentfleet` | any | role |
| `AGENTFLEET_DB_PASSWORD` | none | any | secret |
| `MAX_IN_FLIGHT_TASKS` | `5` | whole number; the pools' rule below | live pods, fleet-wide |
| `MAX_DEFINITION_SESSIONS` | `0` | whole number, `0` to below `MAX_IN_FLIGHT_TASKS` | defined-agent pool inside the cap (`adr/0062`) |
| `MAX_EXTERNAL_SESSIONS` | `0` | whole number, `0` to below `MAX_DEFINITION_SESSIONS` | member and client pool inside that (`adr/0066` §8) |
| `MAX_SESSIONS_PER_MEMBER` | `1` | whole number, at least 1 when `MAX_EXTERNAL_SESSIONS` is above 0 | one member's share of it |
| `STOP_GRACE_MS` | `30000` | whole milliseconds, above 0 | how long a Stop waits before a forced teardown |
| `IDLE_TIMEOUT_MS` | `1800000` | whole milliseconds, above 0 | no transcript activity for this long tears the pod down |
| `STARTUP_STALL_MS` | `180000` | whole milliseconds, above 0 | pod scheduled to first sign of life (`adr/0040`) |
| `TURN_STALL_MS` | `90000` | whole milliseconds, above 0 | owing a response this long reads as `stalled` (informational) |
| `SESSION_RETENTION` | `10d` | Go duration or whole days (`6d`), above 0 | untouched session's disk is reclaimed after this (`adr/0048`); `k8s/core.yaml` sets `6d` |
| `DASHBOARD_PUBLIC_URL` | none | absolute http(s) URL (refuses: malformed, or missing, with the gate on) | console base: the Discord link, the OIDC redirect URI, the accepted origin |
| `OIDC_ISSUER_URL` | none | absolute http(s) URL (refuses: malformed, or missing, with the gate on) | authentik issuer, per application, passed to discovery verbatim |
| `OIDC_CLIENT_ID` | none | any (refuses: missing, with the gate on) | OIDC client |
| `OIDC_CLIENT_SECRET` | none | any (refuses: missing, with the gate on) | secret |
| `FLEET_SESSION_KEYS` | none | any, at least one non-blank key (refuses: missing, with the gate on) | secret, comma-separated cookie keys |
| `FLEET_AUTH_DISABLED` | unset | `1`, or `0` | `1` is the explicit local-stack opt-out of the console gate; any other value warns and the gate stays on |
| `FLEET_VERSION` | none | any | the release reported (`dev` without it) |
| `CLAUDE_MODEL` | none | any | the operator's model override (`adr/0077`) |
| `FLEET_PROVISIONER_TOKEN` | none | any | secret: the provisioner's bearer to `CoreService` |
| `FLEET_CORE_PROVISIONER_TOKEN` | none | any | secret: core's bearer to `ProvisionerService` (`adr/0069`) |
| `FLEET_CLIENT_TOKEN_<NAME>` | none | any | secret: a registered client's bearer; the name is its definition's `tokenRef`, read at match time (`adr/0066` §3) |
| `FLEET_REPO_OVERRIDES` | none | any path | infra-bootstrap's overrides file; empty turns the repos sync off (`adr/0074`) |
| `PROVISIONER_GRPC_ADDR` | `provisioner.agent-fleet.svc.cluster.local:9090` | `host:port` | where core commands the provisioner |
| `STT_ADDR` | `ukubi-stt.ukubi-stt.svc.cluster.local:9090` | `host:port`; read only with `STT_TOKEN_FLEET` set | speech-to-text |
| `STT_TOKEN_FLEET` | none | any | secret; unset disables dictation |
| `LOKI_URL` | in-cluster Loki | absolute http(s) URL | log queries |
| `PROMETHEUS_URL` | in-cluster Prometheus | absolute http(s) URL | the Observability page |
| `GARAGE_S3_ENDPOINT` | `https://s3.bnei.dev` | absolute http(s) URL | files, signed into the presigned URLs: must be browser-reachable |
| `GARAGE_FILES_BUCKET` | `agent-fleet-files` | any | bucket |
| `AGENTFLEET_FILES_S3_ACCESS_KEY` | none | any | secret |
| `AGENTFLEET_FILES_S3_SECRET` | none | any | secret |
| `DISCORD_BOT_TOKEN` | none | any | secret; unset runs without Discord |
| `DISCORD_TRIGGER_CHANNEL_ID` | none | any | channel for "needs a human" posts |
| `ALERT_WEBHOOK_TOKEN` | none | any | secret; unset makes the alert webhook refuse (`adr/0037`) |
| `THOT_DISCORD_CHANNEL_ID` | none | any | passed to the alert webhook, which keeps it for config compatibility |
| `THOT_REPO` | `infra-bootstrap` | any | the repo an alert's proposal is filed against (and its session runs on) |
| `BRIDGE_URL` | none | absolute http(s) URL | the event bridge; unset runs without it (`adr/0078`) |
| `BRIDGE_TOKEN_URL` | none | absolute http(s) URL | authentik's token endpoint, by its in-cluster Service |
| `BRIDGE_AUTH_HOST` | none | host or `host:port` (port 1-65535) | the public Host the token request carries |
| `BRIDGE_CLIENT_ID` | `ukubi-bridge-api` | any | authentik client id |
| `BRIDGE_USERNAME` | `bridge-core` | any | the service account |
| `BRIDGE_PASSWORD_FILE` | `/etc/bridge-core/app_password` | any path | its app password, re-read before each call; absent runs without the bridge |

#### Auth (`core/`, `adr/0056`)

`OIDC_ISSUER_URL` is passed to discovery verbatim (normalising it is a
different issuer, and the path fails closed). `FLEET_SESSION_KEYS`: sign with
the first, verify against any. Unset OIDC config makes core refuse to start
unless `FLEET_AUTH_DISABLED=1`.

### `provisioner/`

`provisioner/internal/config/config.go`. Notable since `adr/0075`:
`GITHUB_APP_DIR` + `FLEET_GITHUB_ACCOUNTS` (the App), `AGENT_VAULT_*` (the
vault), `FLEET_GIT_AUTHOR_*` (repo pods' commit identity). It reads no
`GH_TOKEN` and holds no `AGENTFLEET_DB_*`. `FLEET_CORE_PROVISIONER_TOKEN` is
required (below); it is the only key whose absence stops the start. Every
other variable is optional: one that is set but invalid
(`RECONCILE_INTERVAL_MS` and `SHARED_INSTANCE_IDLE_TIMEOUT_MS` not a positive
integer of milliseconds, `SHARED_INSTANCE_PVC_SIZE` not a positive Kubernetes
quantity) logs a WARN naming the variable and the rule, takes its default and
the process starts; the WARN shows at any `LOG_LEVEL`, and a value containing
`@` is never echoed. One exception, because it is containment rather than
tuning: a *malformed* `FLEET_GIT_URL_REWRITE` refuses the start (dropped, a
kind worker would push to the real github.com through the vault); set against
the production vault it warns and is dropped, which is production's behaviour.

#### SSH access

There is none; `kubectl exec` into the session's pod reaches the same
filesystem (`adr/0048` §6).

### `sidecar/` (per worker pod, injected by the provisioner at pod creation)

`sidecar/internal/config/config.go`, from the env the provisioner sets in
`provisioner/internal/k8s/pod.go`. It holds the session's `LEASE_ID`, which
authenticates every `CoreService` call; missing it, the sidecar refuses to
start.

### `executor/` (thot-executor and the `kubectl` shim, `adr/0037`)

`executor/internal/config/config.go`: `Executor` for thot-executor, `Shim` for
the `kubectl` shim in a cluster-access pod. `THOT_AUTH_TOKEN` is the bearer both
share; the executor refuses to start on one that is empty, blank or shorter than 32
bytes (`newGRPCServer`), the shim exits 2 without `EXECUTOR_ADDR`.

### `worker/` (per worker pod, injected by the provisioner at pod creation)

The top of `worker/src/session.ts`, from the explicit env list in
`provisioner/internal/k8s/pod.go`. `RESUME_FROM_SEQ` is `LatestSeq` at
provisioning time, which is why ordering is warm-then-append.

### `FLEET_PROVISIONER_TOKEN` (`core/` and `provisioner/`)

The one shared secret in the fleet, and the exception that proves the rule.
Every other `CoreService` caller authenticates with its session's own
`lease_id`; the provisioner has none, because it exists before any pod does and
`ReportPodEvents` reports on arbitrary sessions by nature — it is how core
learns a pod exists at all.

Both components already consume the whole `agent-fleet-nygh` scope via
`envFrom`, so adding the key needs no manifest change on either side. Unset, it
authenticates **nobody** (an empty token is not a wildcard) and both processes
say so loudly at startup — necessary because `PERMISSION_DENIED` is not in
either client's retry policy, so the symptom is pod events silently ceasing to
reach core while every log stays green.

### `FLEET_CORE_PROVISIONER_TOKEN` (`core/` and `provisioner/`)

The same shape in the other direction: core's credential to
`ProvisionerService` (`adr/0069`). A separate key rather than a reuse of
`FLEET_PROVISIONER_TOKEN`, so each direction rotates on its own and a leaked
header or log line exposes one direction only. Same whole-scope `envFrom`, so
no manifest change, which also means both keys sit in both pods (and in core's
migrate hook): the split is no protection if either pod leaks its env. Unlike its
sibling, the **provisioner refuses to start** without it. Core only warns:
the console keeps working, and every provisioner call fails `Unauthenticated`.
Worker pods never receive it: the provisioner builds their env from an explicit
list, not `envFrom`.

All of the above flow through Infisical (project `agent-fleet-nygh`,
env `dev`) — never committed, never in a manifest as plain text.

## 8. Current targets

Every repo of the GitHub App `ukubi-fleet`'s allowlisted installations
([`adr/0074`](adr/0074-fleet-repos-come-from-github.md) as amended by
[`adr/0075`](adr/0075-github-app-identity-and-vault-credentials.md) §4),
projected into `repos` each minute. Overrides: infra-bootstrap's
`gitops/bootstrap/fleet-repo-overrides.yaml`. The console's Settings → Repos
toggles `active`. No per-repo Deployment or PVC.

## 9. Relationship to `infra-bootstrap`

- The cluster (`ukubi-cluster`), GitOps (`gitops/`), and secrets backend
  are all owned by `infra-bootstrap` — this repo consumes them, it doesn't
  redefine them.
- Only the Application/ApplicationSet registration lives in
  `infra-bootstrap`'s `gitops/apps/registry.yaml` +
  `gitops/bootstrap/apps.applicationset.yaml` (the latter is the literal
  generator ArgoCD reads; the former is a human-maintained mirror — both
  must be kept in sync manually) — see that repo's `/add-app` skill and
  `gitops/README.md`.
- `core` deploys via this repo's own `k8s/core.yaml` (`common-app-chart`,
  registered in `infra-bootstrap` like any other app) since it needs no
  RBAC `infra-bootstrap` would otherwise have to grant. The ArgoCD
  Application is named `agent-fleet-core` (renamed from `agent-fleet-bot`
  once nothing PVC-stateful was attached to it anymore).
- `provisioner`'s manifests **moved from `infra-bootstrap` into this
  repo** (`k8s/provisioner/`) as part of this redesign — it's still a
  standalone plain-manifest ArgoCD Application (`platform-provisioner` in
  `infra-bootstrap`'s `gitops/bootstrap/provisioner-application.yaml`,
  since it needs RBAC `common-app-chart` can't express), but the
  manifests themselves are no longer `infra-bootstrap`'s to edit.
- This fleet does **not** manage `infra-bootstrap`'s own cluster ops
  (kubespray/ansible/pigsty) — blocked per that repo's own `CLAUDE.md`.
