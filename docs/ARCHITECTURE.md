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
catch-up in v3.0.2. Where they describe a deleted mechanism they now do so in
the past tense and say what replaced it — the queue, the lease/heartbeat/
reclaim machine, worktrees and the e2e sandbox are all gone, and a sentence
here written in the present tense about any of them is a bug in this file.

## 1. Components

| Component | Role |
|---|---|
| `core/` | Go service: Discord notifications (outbound only — the dashboard is the primary surface) + `CoreService`, core's own gRPC server (agent-facing proxied MCP calls, wrapper-facing housekeeping, and the provisioner's pushed pod-lifecycle event stream) + `sessions.Loop`, a 60s reconcile/sweep pass that replaces the dispatch loop, the heartbeat and the reclaim: it syncs `pod_phase` against what Kubernetes actually has (in both directions), enforces stop-grace, startup-stall and idle timeouts, runs the retention GC, and publishes the live-state gauge + transcript coordination (Postgres) + Loki queries + the dashboard's ConnectRPC API and static SPA — one binary. **The fleet's sole holder of `AGENTFLEET_DB_*`** (`adr/0020` point 1) and the *only* caller of the provisioner (hub-and-spoke). Zero cluster RBAC. An `activityTrackingStore` decorator wraps every transcript append to maintain the activity clock the idle sweep and the GC both read. |
| `provisioner/` | Go service (`client-go`) — the only component with Kubernetes RBAC (namespaced `Role`, never a `ClusterRole`). Creates a session's `batch/v1.Job` (`BackoffLimit: 0` — a crashed session is a human's decision to warm again, not a retry) and its own `local-path` PVC, and the `Service` + Traefik `IngressRoute` that `expose()` publishes. Maintains the shared clone cache (`git clone`/`fetch`, `gc.auto=0` so a `--shared` clone's alternates can't be pruned out from under a live session) and seeds each session's own `claude-home` before its pod exists. It does **not** create working trees: that happens in an init container inside the session's pod, the only place both volumes are mounted (`adr/0048` §5). Holds **zero** Postgres credentials; Kubernetes is its durable source of truth. Hosts `ProvisionerService` (core is the only caller) and pushes `PodEvent`s back to core. |
| `sidecar/` | Go — a second container in every session pod (`adr/0020` point 5). Two `localhost`-only surfaces and one outbound gRPC connection to core: an MCP server the Agent SDK connects to (`send_message`, `wait_for_messages`, `AskUserQuestion`, `expose`, `unexpose`, `request_service`, the shared-file tools, `view_logs`, and the inter-agent tools), and a plain HTTP/JSON API for the TS wrapper's control flow (`journal`, `session-id`, `permission-mode`, `telemetry`, `message`) — including the load-bearing `GET /human-messages` SSE feed that delivers new human input to `streamInput()` live. An independent telemetry loop pushes `git diff --numstat` stats every 5s, decoupled from what the agent does. It dials nothing but core: the sandbox it used to reach over MCP (`adr/0045`) no longer exists. |
| `worker/` | The Claude Code worker (TS/Bun — the only JS runtime left, sole host of `@anthropic-ai/claude-agent-sdk`'s `query()`; `src/session.ts`). **Single-shot**: the provisioner hands it one `SESSION_ID`/`TARGET_REPO`/`LEASE_ID`/`RESUME_SESSION_ID` via env, pointed at a `/workspace` an init container already cloned, with `CLAUDE_CONFIG_DIR` on its own per-session volume. Runs **one continuous `query()` in streaming-input mode**, starting in the SDK's `"default"` mode and resuming via `resume:` when `RESUME_SESSION_ID` is set. The input queue is deliberately NOT seeded: there is no fleet-composed prompt, so the human's first message IS the first turn, verbatim. Its image carries the toolchain the sandbox used to (bun, Go, git, gh, Playwright + a real browser), and Playwright runs in-pod on stdio as a second `mcpServers` entry rather than being proxied. Exits explicitly — a single-shot process that merely sets `process.exitCode` can hang forever with the right code and no exit. |
| `proto/` | buf-managed `.proto` schema (lint + breaking-change CI + generate/drift check): `CoreService` (core's gRPC server — agent/wrapper/provisioner-facing), `ProvisionerService` (the provisioner's gRPC server, renamed from `E2eProvisionerService`), and `DashboardService` (ConnectRPC, dashboard ↔ core). Generates Go (`proto/gen/go`) and TS types (`worker/src/gen`, `dashboard/src/gen`, `ts-proto`). |
| `db/migrations/` | Shared Postgres schema (`agentfleetdb`, Pigsty-managed) — sole source of truth, applied via golang-migrate by the dedicated `migration` image on a `PreSync` hook ([`adr/0030`](adr/0030-single-source-schema-via-golang-migrate.md)). Squashed to one `000001_init` pair by `adr/0048`: `sessions` (no status enum, no lease/retry/heartbeat columns), `proposals` (the human gate in front of machine-initiated runs, with a dedup key that re-arms on dismissal or archive), `transcript` (`ON DELETE CASCADE`, which is what let soft-delete die), `repos`, `prompt_snippets`, `scheduled_audits`, `knowledge_journal`. |
| `dashboard/` | React + Vite + TypeScript + Tailwind/DaisyUI SPA — the fleet's **primary** surface, rewritten as a console in [`adr/0042`](adr/0042-console-rewrite.md). Four full-width nav views (**sessions**, **audits**, **files**, **observability**), plus the session detail that replaces the list rather than sitting beside it — the 320px sidebar is gone. The list *is* the fleet overview: a live census, and a blocked session's pending decision rendered **and answerable inline** (an `Edit` as a real diff with allow/deny) off the same per-active-session transcript fetch the todo bars already used — no extra RPC. URL state is `?view=`/`?session=`; the pre-rename `?task=` is still read so links shared before v3.0.0 keep resolving. The detail view is feed · **decision dock** · composer, beside a fixed 266px panel column ([`adr/0043`](adr/0043-one-decision-surface.md) deleted the decision-spine rail: a pending decision now renders in the pinned dock and *only* there, on both form factors, via the shared `DecisionDock`); the feed ranks the twelve entry kinds into five visually distinct tiers (`SessionFeed`), gated by one three-way DENSITY control whose third mode deliberately differs between desktop and mobile (`feedVisibility`). **Mobile is designed, not ported** (`Agent Fleet Console Mobile.dc.html`): persistent top bar + bottom tab bar, bucket filter chips, ~44px targets, the blocking decision **docked above the composer**, and the panels as a bottom sheet — sharing `SessionFeed`/`SessionPanels`/`DecisionInline`/`DecisionDock`, plus the `bucketSessions` partition and the `sessionLabel` fallback, with desktop so the two can't drift. Two themes (`herd` dark default, `herd-light`) from the design tokens, applied pre-paint from `index.html`. **Session creation** (`NewSessionDialog.tsx`): repo alone is enough, title and first message are both optional, and an optional message is sent as a real `PostMessage` after `CreateSession` returns — which is what boots the pod (§4 below). Prompt snippets prefill that message box client-side rather than being sent, since `snippet_ids` is a reserved proto field. Live transcript via a Connect server-streaming RPC; Warm/Interrupt/Kill/**Archive**, a mode picker showing `default`/`plan`/`acceptEdits`/`auto` with the real active mode highlighted, `AskUserQuestion`/`PermissionCard`/`PlanCard` answer forms (`adr/0018`/`0029`), a Loki log drawer, and the **proposals** gate on the audits view — `ListProposals`/`OpenFromProposal`/`DismissProposal`, the human approval in front of every machine-initiated run. **observability** ([`adr/0047`](adr/0047-metrics-scoped-to-the-hubs.md)) is a live fleet topology plus a PromQL explorer: cells are the two hubs and one per live worker pod, coloured by status, and clicking one opens that session — the thing Grafana can't do, which is why the view is deliberately thin next to it and links to it. Hand-laid-out inline SVG, no `d3`/`cytoscape`: two fixed hubs and at most `MAX_LIVE_SESSIONS` cells is a bounded shape whose positions are a loop. Talks to `core` via a generated `@connectrpc/connect-web` client. Built into and served by `core`'s binary — not deployed on its own. |
| `k8s/` | This repo's own deploy manifests: `core.yaml` (Helm values for `common-app-chart`, zero RBAC) and `provisioner/` (a standalone plain-manifest directory — `Deployment`/`Service`/`ServiceAccount`/`Role`/`InfisicalSecret`/`NetworkPolicy`/`PersistentVolumeClaim` — since it needs RBAC `common-app-chart` can't express). Both referenced from `infra-bootstrap`'s `gitops/` (see §9). |
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
    C->>PG: ReserveSlot(sessionId, MAX_LIVE_SESSIONS) — advisory-locked cap, then mint a fresh lease
    C->>P: CreateWorkerPod(sessionId, repo, repoUrl, baseBranch, leaseId, resumeSessionId) [gRPC]
    P->>P: EnsureRepoCloned (clone cache, gc.auto=0) + seed this session's claude-home
    P->>SC: client-go creates the session's PVC, then a batch/v1.Job: clone → sidecar → worker
    C->>PG: Append the message to transcript — AFTER the warm, never before
    P-->>C: ReportPodEvents stream (created→scheduled→running→...→[fast-path Failed], adr/0024) [gRPC]
    C->>PG: SetPodPhase — pod_phase drives concurrency/idle/stop checks now, not status (no journal write, adr/0055)

    W->>Ag: query() streaming-input, permissionMode="default", resume: RESUME_SESSION_ID if set
    Ag->>SC: mcp tool call (local MCP, e.g. send_message)
    SC->>C: SendMessage [gRPC]
    C->>PG: Append to transcript (activityTrackingStore bumps last_active_at)
    C-->>D: relay loop posts allowlisted types only (adr/0025 pt.5) — Discord is notification-only now

    Dash->>C: PostMessage(sessionId, text) — auto-warms an idle session first if needed
    C->>PG: Append (from=human, type=discussion)
    C-->>SC: StreamHumanMessages [gRPC server-stream]
    SC-->>W: GET /human-messages (SSE)
    W->>Ag: streamInput(reply text)

    Ag->>SC: tool call the current permission mode would prompt for
    SC->>C: (via canUseTool, blocked) Append transcript entry, type=permission_request
    C-->>Dash: PermissionCard/PlanCard renders the pending request
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
    C-->>Dash: relay posts the summary
```

Re-opening a session later doesn't restart this diagram from the top: `Warm`
(or the auto-warm inside `PostMessage`) calls the exact same `CreateWorkerPod`,
with `resumeSessionId` set from the session's saved `agent_session_id` — same
volume (still bound, and its node affinity brings the pod back to the same
node), same Claude conversation (resumed via `CLAUDE_CONFIG_DIR` + `resume:`),
new pod. `Warm` rejects with `CodeResourceExhausted` if the fleet is already at
`MAX_LIVE_SESSIONS` live pods.

**The ordering is load-bearing: warm THEN append.** `resumeFromSeq` is computed
from `LatestSeq` at provisioning time, so a message appended before the pod
exists lands below the new pod's cursor and is never delivered — the pod boots
and sits there with nothing to do.

`/stop` (Discord) or the dashboard's Stop button both post a cooperative
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
tool calls, not a hypothetical one. Both Discord and the dashboard can
answer a question; only the dashboard renders/answers a permission
request — `discordSafeTypes` deliberately excludes
`permission_request`/`permission_response` (same poor-fit reasoning
`adr/0018` already gave for `AskUserQuestion`'s structured payload).

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
  publishing the agent's own server at `<shortId>.e2e.bnei.dev`. The agent
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
  for anything not auto-safe (`Read`/`Glob`/`Grep`/`WebSearch`/`WebFetch`
  stay in `allowedTools`, so the SDK never even calls the callback for
  those). `canUseTool`'s only remaining job: post a `permission_request`
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
- **Sessions start in `"default"` mode**, not a forced `"plan"` gate —
  matches running `claude` locally with no flags. `plan` is still fully
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
- **No round cap, no phase boundary — the agent paces itself.** One
  `taskPrompt` spans the whole session; the agent decides when to explore,
  when to plan out loud, and when to implement, ending its final message
  with a `PR_READY:` summary line. That line is now also the session's own
  completion signal for `runSession`'s loop (there's no `approved` boolean
  left to break on) — a "success" result only ends the session once
  `PR_READY:` appears in the agent's own text; anything else means the
  session paused naturally between turns, waiting for the next streamed
  input.
- **`AskUserQuestion`, correlated by sequence and answerable from Discord
  or the dashboard.** A real `AskUserQuestion` MCP tool (proxied through
  the sidecar → `core`) posts a `question`-type `transcript` entry and
  long-polls until a matching `answer` entry appears, threaded via a
  `reply_to_seq` column so a second question posted before the first is
  answered can't have its reply misattributed. `TranscriptEntry.reply_to`
  is now wire-visible (the Go struct already carried it; the proto message
  didn't) — the same correlation now also covers permission requests, not
  just questions.
- **Every raw SDK message is relayed to Discord through an allowlist, not
  a denylist.** `logSdkMessage` relays every SDK message type;
  `relay.go`'s `discordSafeTypes` allowlist decides what actually reaches
  the thread — deliberately still excludes `permission_request`/
  `permission_response` (same poor-fit reasoning `adr/0018` already gave
  for `AskUserQuestion`'s structured payload; the dashboard is the sole
  interactive surface for permission decisions). A denylist would let any
  new raw SDK message type leak by default — a real secret-leak risk (Bash
  stdout/stderr, file contents).
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

- **Discord is outbound-plus-a-link, and nothing else.** `adr/0048` cut it
  back from a second console: the `/task`, `/stop` and `/e2e-kill` slash
  commands, the per-task threads, the relayed free-text replies and the
  retry/dead-letter machine behind them are all gone, leaving one package
  (`core/internal/discord/session.go`) that posts when a session needs a
  human and links to the dashboard.

  The reason is worth keeping written down, because "add buttons to Discord"
  is a permanently tempting idea: **no authorization exists there.** The
  dashboard sits behind Traefik basic-auth; the bot has a token and a channel
  id and no user allowlist, so an interactive control would let anyone who
  can see the channel approve an arbitrary `Bash` or `Edit` on any session.
  `adr/0029` deleted `/approve` deliberately; rebuilding it in Discord is the
  same gate with less checking.
- **Sessions are created from the dashboard, and creating one starts
  nothing** (`adr/0048` §1). `DashboardService.CreateSession` writes a row —
  no pod, no volume, no directory — and the **first `PostMessage` is what
  provisions**. Both calls come from `NewSessionDialog`, in that order, and
  the message is optional: a session with no message is a valid resting
  state, and it is the human gate expressed structurally, since nothing
  machine-initiated produces a message.

  `title`/`description` are human-facing labels the agent never reads. The
  instruction is the first transcript entry, verbatim — sending it as
  `description` instead is a shipped bug, not a shortcut: it lands in a
  column nothing reads, no pod boots, and nothing errors (that was v3.0.0's
  dashboard, fixed in v3.0.2).

  **Ordering is warm-then-append, never the reverse.** `WarmIfIdle` computes
  `resumeFromSeq` from `LatestSeq` at provisioning time, so an entry written
  before the pod exists lands below its cursor and is never delivered.
  `PostMessage`, `OpenFromProposal` and `PromptSession` each carry that
  comment; copy the sequence rather than re-deriving it.

  The dashboard is the primary interaction surface (`adr/0029`); Discord is
  secondary/notification-only, and a dashboard-created session has no thread
  at all — `core/internal/discord/session.go`'s `PostToThread` already
  no-ops on a nil `ThreadID`, so relay works regardless of origin.
- Live relay of every message on the transcript — plan drafts, interview
  questions, doubt-cycle status, raw assistant narration — to the Discord
  thread as it's generated, via `core`'s own relay loop
  (`internal/transcript/relay.go`), regardless of whether the message came
  from the agent's own `send_message` tool call or the sidecar's
  independent telemetry/wrapper housekeeping calls. Permission
  requests/responses are deliberately excluded — dashboard-only.
- Structured self-review via real, vendored Claude Code skills
  (`doubt-driven-development`'s fresh-context `Task`-tool subagent,
  `architecture-interview`'s stakeholder elicitation) — see
  [`adr/0017`](adr/0017-single-session-planning-pipeline.md).
- **One continuous Agent SDK session per warm pod**, streaming-input mode,
  one `taskPrompt`, no fleet-imposed phase boundary between planning and
  implementation — no restart, no context loss within a pod's lifetime.
  `resume: sessionId` is now a real, general-purpose mechanism, not
  crash-recovery-only (`docs/adr/0016`'s originally-described
  `CLAUDE_CONFIG_DIR` redirect is actually wired up as of `adr/0029`) — a
  pod that re-warms an idle session continues the same conversation.
- Live permission model: `canUseTool` reproduces the SDK's own CLI-parity
  tiers via a generic per-tool-call prompt, not a Write/Edit block — see
  §3.
- `/e2e-kill` requests a kill via `core`'s `CoreService.KillE2eEnv`, proxied
  to the provisioner's `ProvisionerService.KillE2ESession` — `core` never
  writes e2e-session state directly, and e2e-session state itself is
  Kubernetes-native now (§6), not a Postgres row.
- **Explicit warm/stop pod lifecycle.** A `Warm` RPC (or `Discuss`'s
  auto-warm on the first message to an idle session) boots a pod for a
  session that already exists but has no live pod right now, threading a
  saved `agent_session_id` through as `resume_session_id`. Pod lifecycle is
  `pod_phase` alone — there is no status to touch — and the concurrency cap
  (`MAX_LIVE_SESSIONS`) is enforced in `ReserveSlot` against a live
  `pod_phase` count, under an advisory lock, on every path including the
  first message.
- **Idle-timeout backstop.** A warm pod with no real transcript activity
  for `IDLE_TIMEOUT_MS` (default 30min) gets torn down automatically —
  same mechanism as a force-stopped pod, gated on `sessions.last_active_at`
  (bumped by an `activityTrackingStore` decorator on every transcript
  append, not the sidecar's git-diff-gated telemetry or the unconditional
  heartbeat timer) rather than a human's explicit Stop.
- Git commit identity derived live from the authenticated bot GitHub
  account (`gh api user --jq .login`), not hardcoded — both the
  provisioner (its own clone/fetch) and the worker (its own push/PR) do
  this independently, since the two processes don't share `$HOME`.
- Append-only `knowledge_journal` — session lifecycle events and what a
  session learned, funneled through `core`'s single Postgres connection,
  readable via a typed `GetJournal` RPC instead of direct-SQL-only
  (`adr/0024`) and, for an agent, via `journal_search` with optional
  repo/query and a `since`/`until` window (`adr/0055`). Not pod
  telemetry: `ReportPodEvents` used to journal every phase transition,
  ~80% of the table with no reader, and stopped (`adr/0055`).
- **A session's disk is its own, and survives teardown.** The working tree and
  dependency caches live on a per-session `local-path` PVC, cloned into by an
  init container in the session's own pod; the SDK resume state is a per-session
  directory on the shared volume. Neither is touched by Stop, by an idle
  timeout, or by a crash — only the retention GC reclaims them, together,
  through `SweepSession`, and only then is `swept_at` written. Worktrees,
  the `agent/<id>` branch convention and the periodic branch sweep are all
  gone (`adr/0048` §5); the agent names its own branch.
- **`expose(port)` publishes the agent's own server**, and
  `request_service(kind)` provisions a shared Postgres/Redis (§2). Between them
  they are what is left of the e2e pod and the recipe system.
- **Fleet-wide concurrency, not one session per repo.** Any number of sessions
  across any known repo can be live simultaneously, up to `MAX_LIVE_SESSIONS`
  (default 5), enforced in `ReserveSlot` under a Postgres advisory lock — CI
  caught 4 tasks claimed with a cap of 2 before that lock existed, because
  under READ COMMITTED every concurrent caller's `count(*)` sees the snapshot
  from before the others committed. A session that cannot get a slot is refused
  outright; nothing queues.
- **Fleet-wide shared file space, backed by Garage S3** (`docs/adr/0030`).
  A flat bucket (`agent-fleet-files`), `core` the sole credential holder —
  it only mints short-lived presigned PUT/GET URLs
  (`ListFiles`/`GetFileUploadUrl`/`GetFileDownloadUrl`/`DeleteFile`, on
  both `CoreService` and `DashboardService`). Agents reach it via four new
  sidecar MCP tools (`list_shared_files`, `get_shared_file_upload_url`,
  `get_shared_file_download_url`, `delete_shared_file`) and move bytes
  themselves via `curl` from Bash — the tools never carry file contents.
  The dashboard's `Files` page does the human-side equivalent, uploading/
  downloading directly against Garage from the browser.
- **Crash reporting, not crash recovery.** Sessions are single-shot
  `batch/v1.Job`s; the instant a Job reaches a terminal phase the
  provisioner's reconcile pass reports it and GCs the Job. Nothing
  re-dispatches: a crashed session surfaces as `pod_phase = CRASHED` with its
  reason attached, and warming it again is a human's decision. The heartbeat,
  the stale-heartbeat reclaim and the `MAX_TASK_RETRIES` cap are all gone
  (`adr/0048`) — they were a retry machine for a pod Kubernetes already
  reports on.

  **BOTH terminal phases report, not just `Failed`.** A `Succeeded` Job used
  to be deleted silently, on the reasoning that a clean exit writes its own
  terminal status first. With status gone that reasoning collapses: this pass
  IS the notification, and a Succeeded Job reporting nothing leaves
  `pod_phase` at RUNNING forever — which the cap counts, wedging the fleet
  after five successful sessions and staying invisible until the sixth.

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
- `worker` container: the TS/Bun image, `SESSION_ID`/`TARGET_REPO`/`LEASE_ID`/
  `SIDECAR_MCP_ADDR`/`SIDECAR_API_ADDR`/`GH_TOKEN`/`WORKTREE_PATH`/
  `CLAUDE_CONFIG_DIR`/`RESUME_SESSION_ID`/`RESUME_FROM_SEQ` env,
  `250m`–`2000m` CPU / `512Mi`–`2Gi` memory.
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
a second, `ClusterIP`-only gRPC port (`CORE_GRPC_PORT`, `9090`, never
publicly routed). Its HTTP ingress is gated behind the shared
`basic-admin-auth` Traefik Middleware (already gating pgweb/Alertmanager).

**`provisioner` needs real RBAC**, so — unlike `core` — it is **not**
deployed through `common-app-chart`. As of this redesign its manifests
moved *into this repo* (`k8s/provisioner/`: `Deployment`, `Service`,
`ServiceAccount`, namespaced `Role`, `InfisicalSecret`, `NetworkPolicy`,
`PersistentVolumeClaim`), registered in `infra-bootstrap` as a standalone
plain-manifest ArgoCD Application (`gitops/bootstrap/
provisioner-application.yaml`) pointed at this repo instead of
`infra-bootstrap/gitops/platform/e2e-provisioner/` (the old location, now
deleted) — see §9. Its `Role` (`k8s/provisioner/role.yaml`) grants
`batch/jobs` alongside `pods`/`services`/`ingressroutes`/`middlewares` as
of `adr/0022` — a gap the fake-clientset unit tests didn't catch, only a
real `kind`-cluster smoke test did.

### Deploy pipeline

1. Push a tag → `.github/workflows/docker.yml`'s single `build-push` job
   builds **all six images** (`core`, `provisioner`, `sidecar`, `executor`,
   `worker`, `migration`) with `buildah` on ukubi-cluster's **`build-runner`
   LXC** and pushes them to the in-cluster Zot registry at
   `registry.bnei.lan:5000` (infra-bootstrap ADR-0034). Every image carries
   one tag — `package.json`'s version, which the job asserts equals the git
   tag, since `deploy` below pins `GITHUB_REF_NAME`.

   One job rather than the old three (`build-push` / `build-push-go` /
   `build-push-migration`) because a self-hosted runner executes one job at a
   time: three jobs with a four-leg matrix meant six serial jobs, each
   re-checking-out and each re-pulling `golang:1.26` over the WAN after the
   previous one's cleanup evicted it. On a PR the paths-filter still narrows
   the list and nothing is pushed. `workflow_dispatch` builds and pushes the
   current `package.json` version without cutting a release.

   A separate `.github/workflows/go.yml` runs the real
   correctness gate for the Go side on every push/PR: `go vet`,
   `golangci-lint`, `go test -race`, `-tags=integration` tests against a
   real Postgres service container, plus `buf lint`/`buf breaking`/a
   generate-drift check for `proto/`.
2. `docker.yml`'s `deploy` job (needs `build-push`) `sed`-bumps every
   `tag: "..."` in `k8s/core.yaml` (Helm-values shape) and every
   `image: repo:tag` in `k8s/provisioner/*.yaml` (plain-manifest shape,
   scoped to
   `registry.bnei.lan:5000/agent-fleet-{core,provisioner,worker,sidecar}` —
   `executor`'s floating `:latest` is deliberately excluded) to the
   pushed tag, and commits straight to `main` (via the default
   `GITHUB_TOKEN`, deliberately not re-triggering `release.yml`'s
   push-to-main trigger). It then `grep`s for the bumped provisioner tag and
   fails if it isn't there: a sed whose pattern stops matching the manifest's
   image prefix is a no-op, and a no-op here means ArgoCD redeploys nothing
   while the workflow goes green.
3. ArgoCD's Applications (`core` via a two-source chart Application,
   `provisioner` via its own standalone plain-manifest Application) pick up
   the new pinned tags and sync.
4. `db/migrations/` is applied via a `PreSync` hook on `core`'s
   Application, running the dedicated `migration` image (`migrate/migrate`
   CLI + `db/migrations/` COPY'd in at build time — see `docs/adr/0030`).
   `core` itself no longer embeds or applies any schema.

`release.yml` runs separately (`release-it`, conventional-changelog/
angular preset) on ordinary pushes to `main`, bumping `package.json`'s
version and `CHANGELOG.md` — unrelated to the image-tag bump above; the
whole fleet (`core`/`provisioner`/`sidecar`/`worker`/`e2e-runner` images)
still ships as one version/CHANGELOG, one repo.

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
| `core` | `:8080` (alongside `/healthz` and the dashboard) | ServiceMonitor in `k8s/core.yaml`'s `extraManifests` |
| `provisioner` | `:8080` | `k8s/provisioner/servicemonitor.yaml` |

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

`core` and `provisioner` carry their version compiled in
(`-ldflags "-X main.version=$VERSION"`, with `docker.yml` passing the same
string it tags the image with, so binary and tag cannot drift). `worker` and
`sidecar` get no such stamp: their version *is* the image tag the provisioner
puts in the pod spec, which is the truth actually deployed. The dashboard SPA
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

```mermaid
erDiagram
    sessions {
        uuid id PK
        text repo "name in repos table, adr/0028"
        text title "human-facing label, optional — never part of the prompt"
        text description "legacy label, nullable; new sessions leave it empty"
        text agent_session_id "the SDK's own resume id — renamed from session_id now that the row IS a session"
        text model
        uuid lease_id "minted per pod; stops a dying pod overwriting its replacement's resume identity"
        text permission_mode "NOT NULL DEFAULT 'default' — CLI parity with nothing to forget"
        text pod_phase "the fleet's only liveness signal; reconciled against Kubernetes every 60s"
        text pod_message
        text last_error
        timestamptz last_active_at "activity clock — feeds the idle sweep, the GC and DeriveLiveState"
        text last_entry_type
        text last_entry_from
        boolean activity_seen "real latch, reset on provision — a derived version never fires twice for a resumed session"
        timestamptz seen_at "what distinguishes idle from done"
        timestamptz stop_requested_at "when a human first asked to stop; the grace sweep force-kills off this"
        text worker_image "NOT NULL DEFAULT '' — the image this session's pod actually got, recorded at dispatch"
        text sidecar_image "NOT NULL DEFAULT '' — likewise; both stay true after the fleet is upgraded underneath the session"
        timestamptz swept_at "retention GC reclaimed the disk — readable, not resumable"
        timestamptz archived_at "the human is finished; the only terminal state a machine cannot compute"
        timestamptz created_at
        timestamptz updated_at
    }
    proposals {
        uuid id PK
        text repo
        text source "alert | audit"
        text dedup_key "unique per repo while open — re-arms on dismissal"
        text title
        text body "the instruction; OpenFromProposal sends it as the first MESSAGE, never as description"
        uuid session_id FK "NULL until a human opens it"
        timestamptz dismissed_at
        timestamptz created_at
    }
    transcript {
        uuid session_id FK
        bigint seq PK
        text from "agent|human|session"
        text text
        text type "discussion|abort|interrupt|question|answer|tool_call|system|assistant|user|result|permission_mode|permission_request|permission_response"
        text idempotency_key
        bigint reply_to_seq "answer/permission_response's back-reference to its question/permission_request"
        timestamptz notified_at "one nullable timestamp replaces the old four-column relay/dead-letter machine"
        timestamptz created_at
    }
    repos {
        text name PK
        text url
        text base_branch "'' means provisioner defaults to main"
        text image "'' means the fleet default — replaces adr/0034's recipe ingredients"
        boolean cluster_access "the kubectl shim to thot-executor (adr/0037) — a privilege grant, not a toolchain"
        timestamptz created_at
        timestamptz updated_at
    }
    prompt_snippets {
        uuid id PK
        text name UK
        text text
        text suggested_permission_mode "NULL means no suggestion"
        timestamptz created_at
        timestamptz updated_at
    }
    scheduled_audits {
        uuid id PK
        text name UK
        text prompt
        int interval_seconds
        boolean enabled
        timestamptz next_run_at
        timestamptz last_run_at
        text last_status
        timestamptz created_at
        timestamptz updated_at
    }
    knowledge_journal {
        bigserial id PK
        text repo
        text actor "worker | provisioner | sidecar | core"
        text event_type "session.*|pod.<phase>|..."
        jsonb payload
        timestamptz created_at
    }
    sessions ||--o{ transcript : "session_id, ON DELETE CASCADE"
    sessions ||--o| proposals : "session_id, ON DELETE SET NULL"
```

`knowledge_journal` is deliberately **not** FK'd to `sessions`: it outlives
them, which is the point of an append-only fleet memory.

`sessions` is the durable record of one conversation on a repo. It is not a
queue: `adr/0048` deleted `status` (all eight values), `heartbeat_at`,
`retry_count`, `claimed_by`, `pr_url`, `awaiting_human` and the four `relay_*`
columns. `pod_phase` is the single "is there a live pod right now" source of
truth that `Warm`, `Stop`, the sweeps and the concurrency cap all read; it is
reconciled against Kubernetes every 60s in both directions, because a pushed
pod event can be dropped and a dropped terminal event would hold a concurrency
slot forever.

Two deletions are worth naming because they each needed a replacement rather
than just removal:

- **`awaiting_human` → a transcript predicate.** It was one boolean, but
  permissions are a LIST — parallel tool calls each get their own `seq` — so
  any single `permission_response` cleared the flag for all of them, marking a
  session unblocked while others were still waiting. It is a `pending_decisions`
  count computed by a correlated subquery now.
- **`pr_url` → nothing.** Its only writer was the worker's terminal status
  write, which never passed it, so the dashboard's PR badge had rendered `null`
  for every session in the fleet's history. The dashboard links a GitHub search
  on the branch instead: no column, no writer, cannot go stale.

`proposals` is the human gate in front of machine-initiated runs (alerts,
scheduled audits). Its dedup window is "not dismissed", deliberately not "not
yet opened" — keyed on un-opened, the key would free the moment a human clicks
approve, so a 1-hour audit cadence whose session runs 3 hours files three
proposals for the same work. Archiving the session it produced is what re-arms
it.

`knowledge_journal` is append-only, written only by `core` — the worker and
its sidecar go through `CoreService.AppendJournal`, since `core` is the
fleet's sole Postgres-credential holder. `ReportPodEvents` is deliberately
**not** on that list any more (`adr/0055`): it wrote one row per pod phase
transition, ~80% of the table, that nothing ever read — the live value is
`sessions.pod_phase`, written by the same handler, and the history is in
Loki. `transcript` gives append-once-per-call ordering plus real dedup via
the `(session_id, idempotency_key)` unique index, and `ON DELETE CASCADE` —
that FK, together with `proposals`' `ON DELETE SET NULL`, is the entire reason
`deleted_at`/soft-delete could die.

`repos` is the dashboard-editable target-repo config (`adr/0028`) — no
`sessions.repo` FK, deliberately, so deleting a repo doesn't retroactively
break historical rows. It gained `image` (the container image a repo's sessions
run in, replacing three profile tables) and `cluster_access` (a privilege
grant, not a toolchain — `adr/0037`).

**`e2e_sessions`, `repo_profiles`, `repo_profile_tools` and
`repo_profile_services` are gone**, along with the 14 migration files that
built them: the schema is squashed to one `000001_init` pair. The e2e session
table had been dead since `adr/0020` point 1 made Kubernetes the source of
truth for pod state; the three profile tables stored, in Postgres, what the
agent can read off the working tree it is already sitting in.

## 7. Environment variables

### `core/`

| Var | Default | Notes |
|---|---|---|
| `CORE_PORT` | `8080` | HTTP: `/healthz`, dashboard ConnectRPC API, static SPA |
| `CORE_GRPC_PORT` | `9090` | `CoreService` — the provisioner's `ReportPodEvents` client and every worker pod's sidecar connect here |
| `GARAGE_S3_ENDPOINT` | `https://s3.bnei.dev` | Must stay externally reachable, not `garage.bnei.lan` — presigned URLs sign the endpoint host in, and the dashboard's browser can't resolve a `.lan` name (`docs/adr/0030`) |
| `GARAGE_FILES_BUCKET` | `agent-fleet-files` | The fleet-wide shared file space's one bucket |
| `AGENTFLEET_FILES_S3_ACCESS_KEY` / `_SECRET` | – | `core`'s the sole holder — it only mints presigned PUT/GET URLs, never proxies bytes (`docs/adr/0030`) |
| `AGENTFLEET_DB_HOST`/`PORT`/`NAME`/`USER`/`PASSWORD` | `postgres.bnei.lan`/`5432`/`agentfleetdb`/`dbuser_agentfleet`/– | the *only* component in the fleet with these |
| `DISCORD_BOT_TOKEN` | – | |
| `DISCORD_TRIGGER_CHANNEL_ID` | – | |
| `LOKI_URL` | `http://loki.monitoring.svc.cluster.local:3100` | log/introspection queries |
| `PROMETHEUS_URL` | `http://platform-prometheus-kube-p-prometheus.monitoring.svc.cluster.local:9090` | backs the dashboard's Observability page (`adr/0047`). `core` proxies PromQL because Prometheus has no IngressRoute — a browser can't reach it. Empty disables the page's queries without affecting anything else |
| `PROVISIONER_GRPC_ADDR` | `provisioner.agent-fleet.svc.cluster.local:9090` | `ProvisionerService` client target |
| `MAX_IN_FLIGHT_TASKS` | `5` | max LIVE PODS fleet-wide, enforced in `ReserveSlot` against a live `pod_phase` count under a Postgres advisory lock. Not a queue depth — nothing queues; a session that cannot get a slot is refused |
| `STOP_GRACE_MS` | `30000` | how long the session loop waits after a Stop request before force-tearing down a pod that hasn't gone terminal on its own |
| `IDLE_TIMEOUT_MS` | `1800000` (30min) | a live pod with no real transcript activity this long is torn down. The session survives and stays resumable |
| `STARTUP_STALL_MS` | `180000` (3min) | a pod that came up and never said anything is torn down (`adr/0040`), gated on `activity_seen` — which `ReserveSlot` resets per pod, so it cannot be derived from the transcript |
| `SESSION_RETENTION_MS` | `1209600000` (14d) | the retention GC: a session idle this long with no live pod has its PVC and SDK state reclaimed and `swept_at` written. The row and transcript survive — a swept session is readable history, just not resumable |
| `TURN_STALL_MS` | – | how long a session may owe a human a response before `DeriveLiveState` reports `stalled` |

### `provisioner/`

| Var | Default | Notes |
|---|---|---|
| `NAMESPACE` | `agent-fleet` | where it creates/deletes Jobs, PVCs, Services and IngressRoutes |
| `WORKER_IMAGE` | `registry.bnei.lan:5000/agent-fleet-worker:latest` | pinned by the deploy job in practice. `registry.bnei.lan:5000` is ukubi-cluster's own Zot registry (infra-bootstrap ADR-0034) — LAN-only plain HTTP, anonymous pull, so no `imagePullSecrets` |
| `SIDECAR_IMAGE` | `registry.bnei.lan:5000/agent-fleet-sidecar:latest` | pinned by the deploy job in practice |
| `WORKSPACE_PVC` | `agent-fleet-workspace` | the shared RWX volume: the clone cache and per-session `claude-home` |
| `WORKSPACE_ROOT` | `/workspace` | where that volume is mounted inside the provisioner's own pod |
| `SESSION_STORAGE_CLASS` | – | class for per-session working volumes. **Empty means the cluster default, which on ukubi-cluster is `longhorn` — not node-local.** Set it to a `WaitForFirstConsumer` local class to get the behaviour `adr/0048` §4 measured |
| `E2E_HOST` | `e2e.bnei.dev` | wildcard **base domain**, not a host: `expose()` publishes a session at `<shortId>.e2e.bnei.dev`, serving at `/` with nothing stripped ([`adr/0038`](adr/0038-per-task-subdomain-e2e-preview.md)). One `*.e2e.bnei.dev` cert via Traefik's `le-dns` DNS-01 resolver, defined in `infra-bootstrap`. The per-repo `E2E_START_CMD_*` vars are gone — the fleet does not know how to start anything |
| `PORT` | `8080` | HTTP (currently unused beyond health, kept for parity) |
| `GRPC_PORT` | `9090` | `ProvisionerService` |
| `CORE_GRPC_ADDR` | `agent-fleet-core.agent-fleet.svc.cluster.local:9090` | where `ReportPodEvents` streams to |
| `RECONCILE_INTERVAL_MS` | `10000` | terminal-Job GC + idle shared-instance GC poll (`internal/reconcile`) |
| `GH_TOKEN` | – | for the provisioner's own clone/fetch auth (`gh auth setup-git`), and forwarded verbatim into every worker pod's `worker` container env |

No `AGENTFLEET_DB_*` here at all — see §6.

#### SSH access

There is none. It was a code-server/sshd surface on the e2e pod, for human
debugging over `kubectl port-forward` with VSCode Remote-SSH; both the pod and
its image are gone (`adr/0048` §6). `kubectl exec` into the session's pod
reaches the same filesystem, and `expose()` publishes whatever the agent is
running.

### `sidecar/` (per worker pod, injected by the provisioner at pod creation)

| Var | Default | Notes |
|---|---|---|
| `TASK_ID` | *(required)* | |
| `TARGET_REPO` | – | |
| `CORE_GRPC_ADDR` | `agent-fleet-core.agent-fleet.svc.cluster.local:9090` | its one outbound gRPC connection |
| `WORKTREE_PATH` | `/workspace` | for the telemetry loop's `git diff`/`rev-parse` calls |
| `MCP_PORT` | `9090` | agent-facing local MCP server |
| `LOCAL_API_PORT` | `9091` | wrapper-facing plain HTTP/JSON API |

### `worker/` (per worker pod, injected by the provisioner at pod creation)

| Var | Default | Notes |
|---|---|---|
| `SESSION_ID`, `TARGET_REPO`, `LEASE_ID` | *(required)* | `LEASE_ID` is minted per pod by `ReserveSlot`, not by a claim — nothing claims anything (`adr/0048` §2). It is what `StillHoldsLease` checks before a push, so a torn-down pod still shutting down cannot overwrite its replacement's resume identity |
| `SIDECAR_MCP_ADDR` | `localhost:9090` | the Agent SDK session's `mcpServers` config points here |
| `SIDECAR_API_ADDR` | `localhost:9091` | the wrapper's own control-flow calls |
| `WORKTREE_PATH` | `/workspace` | Kept its name through `adr/0048`; it is a plain clone on the session's own PVC now, not a linked worktree |
| `CLAUDE_CONFIG_DIR` | per-session | redirects the SDK's session-state directory onto durable storage — without this, `resume:` has nothing to resume from regardless of `RESUME_SESSION_ID` (`adr/0029`, completing the redirect `adr/0016` described but never wired up). Per-session as of `adr/0048`, not one directory shared by every pod |
| `RESUME_SESSION_ID` | `""` | non-empty when this pod is warming an existing session — set from `sessions.agent_session_id`, passed as `resume:` into `query()` (`adr/0029`) |
| `RESUME_FROM_SEQ` | `0` | the transcript cursor this pod streams from, computed as `LatestSeq` at provisioning time. **This is why ordering is warm-then-append**: a message written before the pod exists lands below this and is never delivered |
| `GH_TOKEN` | – | the worker's *own* `git push`/`gh pr create` auth — separate from the provisioner's, since the two are different pods that don't share `$HOME` |
| `CLAUDE_MODEL` | `claude-opus-4-8` | |
| `MAX_TURNS` | unbounded | opt-in cap |
| `CLAUDE_CODE_OAUTH_TOKEN` | – | minted via `claude setup-token` |

All of the above flow through Infisical (project `agent-fleet-nygh`,
env `dev`) — never committed, never in a manifest as plain text.

## 8. Current targets

`dream-analyst`, `vos-monolith`, `agent-fleet` — real repos, seeded into
the `repos` table (see §6, `docs/adr/0028`). The `repos` table is the
source of truth for which repos a session can target, and their
per-repo config (clone URL, base branch), dashboard-editable at
runtime — no redeploy needed, and no Go source change either (superseding
the old `core/internal/tasks.KnownRepos` map, itself moved from
`bot/src/db.ts`'s `KNOWN_REPOS`, then from `fleet-core`). No per-repo
Deployment or PVC exists — onboarding a new repo is a dashboard "manage
repos" entry, not new k8s manifests (`docs/adr/0019`'s stated goal, now one
step further via `docs/adr/0028`).

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
