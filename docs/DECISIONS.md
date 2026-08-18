# DECISIONS

Settled-decisions log for `agent-fleet`. This is the WHY: rationale for
choices that were never really in question, plus a quick-reference list of
things not to propose. It is **not** a spec doc — topology and current
features live in [`ARCHITECTURE.md`](ARCHITECTURE.md). Full alternative-
weighing for anything that had real competing options lives in
[`adr/`](adr/README.md), one file per decision, independently trackable by
status.

Any doc, code, comment, or memory that contradicts this file or an
`Accepted` ADR is overridden by them until the file/ADR itself is updated.

## Reading order (for AI agents)

1. This file (`DECISIONS.md`) — hard prerequisite.
2. [`ARCHITECTURE.md`](ARCHITECTURE.md) for topology/current features.
3. [`adr/`](adr/README.md) for the reasoning behind any specific decision.
4. Then the source itself (`core/internal/`, `provisioner/internal/`,
   `sidecar/internal/`, `worker/src/`).

---

## 1. Locked decisions (no dedicated ADR — never really in question)

- **Bun is the sole JS runtime for `worker/`** — the only remaining TS/Bun
  component (superseded 2026-08-03 by [`adr/0013`](adr/0013-go-fleet-core-and-e2e-provisioner-rewrite.md)
  for `bot/`/`mcp-redis/`, which no longer exist; `core`/`provisioner`/
  `sidecar` are all Go, per [`adr/0019`](adr/0019-shared-pvc-and-unified-provisioner.md)–[`0021`](adr/0021-continuous-streaming-session.md)'s
  rename/redesign of `fleet-core`/`e2e-provisioner` plus the new `sidecar`
  component). No build step for `worker/`'s TypeScript sources — runs
  directly. No npm/pnpm/yarn workspace tooling; `worker/` keeps its own
  `package.json`/`bun.lock`, and each Go module keeps its own `go.mod`
  under the shared `go.work`.
- **Git auth goes through the `gh` CLI**, not a hand-rolled token-in-header
  client — `gh auth setup-git` wires `GH_TOKEN` into git's credential
  helper once, and `gh pr create`/`gh api` reuse the same token. Both the
  provisioner (its own clone/fetch on the shared PVC) and the worker (its
  own push/PR) configure this independently — separate pods, no shared
  `$HOME`.
- **`knowledge_journal` is append-only**, not a mutable shared doc — avoids
  write-conflict issues a shared mutable record would hit across
  concurrently dispatched worker pods. Written only by `core` now (`core`
  is the fleet's sole Postgres-credential holder,
  [`adr/0020`](adr/0020-hub-and-spoke-grpc-worker-sidecar.md) point 1) —
  every other component appends to it via a `CoreService` gRPC call, not a
  direct write.
- **The journal holds agent knowledge, and nothing writes to it
  automatically**
  ([`adr/0055`](adr/0055-the-journal-is-agent-knowledge-not-pod-telemetry.md)).
  `ReportPodEvents` used to append a row per pod phase transition and the
  worker a `session.started`/`stopped`/`failed` row per run — together ~96% of
  the table, read by nothing, each duplicating something already in
  `sessions.pod_phase`, `transcript` or Loki. Both are gone; a crash's error
  moved to the transcript, which is the surface the dashboard actually
  renders. What remains is `agent_note`: a session deliberately writing what
  the next session on that repo needs to know. **Lifecycle bookkeeping is not
  knowledge — if a new event type is machine-generated, it belongs in
  Loki/Prometheus, not here.** Reading it is `journal_search(repo?, query?,
  since?, until?, limit?)` — every argument optional, because "all repos, last
  seven days" is the question it exists to answer.
- **One repo, one version/CHANGELOG.** `release.yml` runs from the repo
  root, not per-package — the fleet ships as one unit even though
  `worker/`, `core/`, `provisioner/`, `sidecar/` are deployed as separate
  images. As of the Zot cutover this is enforced rather than assumed:
  `docker.yml` tags all six images with `package.json`'s version and fails
  the build if it disagrees with the git tag, because `deploy` pins
  `GITHUB_REF_NAME` into the manifests. Previously `worker` read
  `package.json` while the Go and migration builds used the ref name — the
  same value on a release-it release, so the split was invisible.
- **Images come from ukubi-cluster's own registry, built on its own
  hardware.** `registry.bnei.lan:5000` (Zot, Garage-backed), built with
  `buildah` on the `build-runner` LXC — never Docker Hub, never in-cluster
  (buildah cannot extract layers without `CAP_SYS_ADMIN`). LAN-only plain
  HTTP with anonymous pull, so no `imagePullSecrets` anywhere. The registry
  keeps the **last 3 tags** per image plus `latest`; `executor`'s floating
  `:latest` is depended on by `catalog.go` and by infra-bootstrap's thot
  manifest, and is pinned in the retention policy for that reason. See
  infra-bootstrap ADR-0034.
- **Workflow discipline:** all changes via feature branch + PR, no direct
  push to `main`; secrets only via Infisical, fetched at run time, never
  committed.
- **`core` is the sole holder of the Garage S3 credential** backing the
  fleet-wide shared file space — it only ever mints short-lived presigned
  PUT/GET URLs, never proxies file bytes itself. See
  [`adr/0031`](adr/0031-garage-s3-shared-files.md).
- **`thot` is an ordinary worker session, not a special component**
  ([`adr/0037`](adr/0037-thot-is-a-worker-task.md), superseding
  `adr/0035`). A thot session is a worker session on a repo whose `repos`
  row carries `cluster_access`; nothing in the create or provision path
  knows it is special, and `Session.kind` is a reserved proto field with no
  writer. Cluster privilege lives in `thot-executor`, a standing service
  holding the ClusterRole — thot pods themselves hold **zero Kubernetes
  credentials**, restoring `adr/0012`'s rule that the component holding
  write-in-git trust never also holds infra-mutation trust. There is **no
  hub-and-spoke exception**: `adr/0020` point 5 holds unqualified again.
  (`adr/0045` briefly made direct dial the general rule so the sidecar could
  reach its own sandbox; with the sandbox merged into the worker pod by
  [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md) there is no
  cross-pod dial left, and point 5's "one upstream channel" is once again
  literal — one outbound gRPC connection, MCP entirely on `localhost`.
  `thot-executor` remains the one direct path, and it is ordinary because it
  needs no `core` state.)
- **An RPC returns nothing, or the one thing the caller cannot already
  know.** A successful Connect/gRPC call already carries "it worked" in its
  error channel; a `status` field inside the response body is a second
  channel saying the same thing, and two channels means every caller has to
  check both until one of them checks only the wrong one. So an
  acknowledgement is `message FooResponse {}`, not `{status: "ok"}`.

  Where two RPCs genuinely perform the same operation they share one
  response message rather than growing a near-duplicate: `SendMessage`,
  `PostMessage`, `AnswerQuestion` and `RespondToPermission` all append a row
  to `transcript`, so all four return `AppendResponse{seq}` — the seq being
  the correlation key nothing else can compute. buf's default is one
  response type per RPC; overriding it with an explicit
  `buf:lint:ignore RPC_REQUEST_RESPONSE_UNIQUE` is fine when the shapes are
  genuinely one shape, and wrong when they merely look alike today.

  **Rejected: a single universal response type for every RPC.** It reads
  simpler and is worse — folding `AskUserQuestion` and `PromptSession` in
  (both return more than a seq) produces a bag of optional fields where
  nothing says which are set for which call, and a `oneof` or `bytes
  payload` throws away the typed accessors that are the whole reason for
  codegen.

  Enforced by `core/internal/buildguard`'s `TestResponseMessagesCarryInformation`,
  because this one regressed silently: adding `string status = 1` compiles,
  lints, and looks exactly like the response next to it. docs/adr/0048 found
  10 ceremonial responses against 7 already-empty ones, picked between at
  random — and the guard found an 11th in `files.proto` on its first run.
  The rule deliberately does **not** ban `status` outright: `GetE2eStatus`'s
  reports real pod state. It catches only a response whose *entire* content
  is an acknowledgement.
- **A session is the unit, and the first message boots the pod.**
  `CreateSession` makes a row and nothing else; `SendMessage` provisions on
  demand and *then* appends. There is no queue, no lease claim, no retry
  counter and no `status` enum — liveness is `pod_phase`, reconciled against
  Kubernetes every 60s. Ordering is **warm-then-append, never the reverse**:
  `resumeFromSeq` is computed at dispatch, so a message appended before the
  pod exists lands below its cursor and is never delivered. A session with no
  message has no pod, which is what makes "a machine may propose, never open"
  structural rather than conventional. See
  [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md).
- **One pod per session, holding the agent and its app.** Builds, tests and
  installs are native `Bash`; the un-prompted set lives in allow-rules in
  `fleet-shared/settings.json`, the same file a CLI user edits, rather than
  being a property of which pod a command lands in. The agent starts and stops
  its own server and calls `expose(port)` to get an HTTPS URL — so the fleet
  no longer stores how to build or run anything. `request_service(kind)` stays
  fleet-side because it needs cluster RBAC and the agent has none. See
  [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md).
- **`allowedTools` is load-bearing and stays.** The SDK's MCP tools return
  `behavior: "passthrough"` and its evaluator converts passthrough to `ask`,
  so an absent allowlist would gate `send_message` and the MCP
  `AskUserQuestion` — the agent would need permission to ask for permission.
  The `mcp__agent-fleet-sidecar__*` wildcard covers every present and future
  sidecar tool; explicit per-tool entries beside it are redundant, not
  protective.
- **The fleet leaves the git business.** Per session the provisioner fetches
  the shared clone cache, runs one `git clone --shared` into the session's
  directory, and seeds its config dir. No worktree, no branch, no naming
  convention, no sweep — the agent runs `git checkout -b` and `gh pr create`
  itself. `gc.auto=0` on the cache clones, because a `git gc` there can prune
  objects a live session's `alternates` still references. See
  [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md).
- **Storage is split by access pattern, not by uniformity.** Two volumes,
  four subPath mounts: a per-session **RWO volume on a node-local class**
  (`local-path`) carrying `/workspace` and `/cache`, and the shared **RWX
  volume** carrying `/repo-cache` read-only and `/claude-home/<id>` private to
  one session. This is the first time session isolation is a mount boundary
  rather than a directory-naming convention, and it is what stops
  `SyncFleetShared` rewriting `settings.json` and `skills/` under sessions
  that are mid-turn.
  <br>The caches are **per session, not global** — an earlier draft of this
  entry said otherwise. A per-node shared cache is not a shape Kubernetes
  offers, and sharing was never where the measured win came from: node-local
  disk is 107x on bandwidth and ~200x on metadata, so a brand-new session
  installs cold in seconds rather than warm in minutes. Sessions are pinned
  to nodes big enough to hold a working tree (`SESSION_NODE_SELECTOR`), since
  `local-path` is a hostPath on the node's OS disk and its size request is
  advisory — `SESSION_RETENTION_MS` is what actually bounds the disk. See
  [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md).
- **Prometheus metrics come from `core` and the provisioner only; worker and
  sidecar telemetry stays in Loki.** Worker pods are single-shot Jobs that
  routinely start and exit between two 30s scrapes, so a counter on them
  samples a random subset of sessions — authoritative-looking and wrong. Loki
  keeps every line regardless of pod lifetime, and `session.ts` already logs
  the SDK's turns/cost/tokens as structured fields. Scraping is by
  **ServiceMonitor**, never `prometheus.io/*` annotations: this cluster's
  Prometheus is the Operator with no `additionalScrapeConfigs`, so
  annotations are inert while looking like configuration. No metric carries
  a `session_id` label. See
  [`adr/0047`](adr/0047-metrics-scoped-to-the-hubs.md).
- **A session loads `settingSources: ["user", "project"]` — never
  `"local"`.** `"user"` is the provisioner-synced `fleet-shared/` context;
  `"project"` is the target repo's own `CLAUDE.md` and `.claude/skills/`,
  which no worker could see until `adr/0049` (the "target repo's CLAUDE.md
  wins" line in `fleet-shared/CLAUDE.md` described a file the session was not
  loading). `"local"` stays out because `.claude/settings.local.json` is
  gitignored, so nothing about it is reviewable in the PR that lands it.
  `"project"` also merges that repo's `permissions.allow`, which is why the
  fleet carries an `ask` block of its own (in `fleet-shared/settings.json`
  when this was decided, in `worker/src/session.ts`'s `FLEET_ASK_RULES` since
  `adr/0052` — see the next bullet): the SDK
  resolves **deny → ask → allow** and returns on the first match, so a
  user-scope `ask` outranks a project-scope `allow`. `ask` rather than `deny`
  because a human must still be able to approve a `git push` — every result
  is a PR. There is no specificity tiebreak, so a broad `ask` prefix swallows
  every narrower `allow` beneath it. See
  [`adr/0049`](adr/0049-project-setting-source.md).
- **`auto` is the mode plan approval switches to**, and every mode switch is a
  live control request. A rejected switch is reported to the human instead of
  being logged and silently treated as an allow. The `permissions.ask`
  counterweight above lives in `worker/src/session.ts`'s `FLEET_ASK_RULES`,
  injected through the SDK's `settings` option.
  `allowDangerouslySkipPermissions` is still never passed: it would make plan
  mode auto-allow every write from turn one. See
  [`adr/0052`](adr/0052-auto-mode-and-the-bypass-launch-profile.md).
- **The permission gate is `canUseTool`, not a rule list the SDK
  re-interprets.** Two things are decided in the fleet's own process, above
  whatever the evaluator does: the fleet's **own** tools
  (`mcp__agent-fleet-sidecar__*`, `mcp__playwright__*`) are always allowed —
  otherwise the agent needs permission before it can *ask* for permission,
  which is what shipped — and in `auto` everything is allowed except a `Bash`
  running `rm` or `sudo`. `allowedTools` still lists the MCP tools and is
  still the fast path, but nothing depends on it: 0.3.233 asks for every
  non-read-only MCP tool in `plan` mode, and again under an org-level
  `effectiveMaxPermission` ceiling, both **above** the allow-rule lookup.
  `bypassPermissions` is deleted — it bought only `rm`/`sudo` over `auto`, and
  charged a launch profile for them. See
  [`adr/0053`](adr/0053-the-gate-is-canusetool-not-a-rule-list.md).

## 2. Forbidden patterns (quick check — full list + reasons in `adr/`)

- **A session able to read or write another session's working tree.** One
  private directory per session, enforced by the **mount**, not by naming.
  This was violated for most of the fleet's history: the whole-PVC mount
  existed because a linked worktree's `.git` is an absolute-path gitlink that
  a subPath severs, so isolation was directory naming only — see `adr/0048`.
- **Inferring a permission decision from silence, round completion, or
  free-text sentiment.** `canUseTool` prompts live and blocks for a real,
  structured `RespondToPermission` reply (or an explicit
  `SetPermissionMode` call, itself confirmation-gated for `auto`) — never
  inferred from anything else. `/approve` no
  longer exists — see `adr/0005`, `adr/0027`, `adr/0029`.
- **Reclaiming a session's directory on anything but archive or the
  retention timer.** Stop and idle-timeout tear down the pod and leave the
  tree — that is what makes a session resumable. The underlying lesson from
  `adr/0023` outlives the worktree machinery it was written about:
  uncommitted work was destroyed twice by a design that tied git state to a
  lifecycle event — see `adr/0048`.
- **A `status` field that only means "it worked".** An acknowledgement
  response is `{}`; the error channel already carries success. Guarded by
  `buildguard.TestResponseMessagesCarryInformation` — see §1.
- **The fleet storing what the agent can read off the repo.** Start commands,
  toolchain recipes and profile tables all encoded knowledge that lives in
  the working tree the agent is already sitting in. `request_service` is the
  carve-out and the test for new ones: it stays because it needs cluster RBAC
  the agent does not have — see `adr/0048`.
- **Anything inside the e2e container deciding to end the pod.** PID 1
  outlives every child; pod lifetime belongs to `kill_env`, core's teardowns
  and the reconcile sweep — see `adr/0044`.
- **Hardcoding git commit identity.** Always derived live from the
  authenticated bot GitHub account — see `adr/0006`.
- **A declared-but-never-incremented metric.** A metric that renders as
  absent is indistinguishable from a broken feature, and shipping twenty of
  them is how the first attempt at the observability layer passed review.
  Every metric is incremented at a real call site, and verification means
  seeing a non-default value on a live `/metrics` endpoint — not that the
  code compiles — see `adr/0047`.
- **Granting `core` cluster RBAC to render something.** The topology view
  gets its cells from `sessions.pod_phase`, which `ReportPodEvents` already
  maintains; a picture is not a reason to break `adr/0020` point 1 — see
  `adr/0047`.
- **A page-load failure rendered as a blocking modal.** The mobile tab bar is
  the only way out of a view, so a modal over a failed page trapped the user
  entirely when the provisioner or object store was unreachable. Load errors go
  inline (`InlineError`) with a retry; modals are for decisions — see
  `adr/0042`.
- **Giving a new transcript entry kind the same weight as everything else.**
  The feed's five tiers are the whole point of the rewrite: a `$0.42 · 7 turns`
  line and the agent's prose reading identically is the failure it fixes. A new
  kind is placed in a tier — see `adr/0042`.
- **Duplicating feed, panel or decision rendering between desktop and mobile.**
  They share `SessionFeed`/`SessionPanels`/`DecisionInline`/`DecisionDock`,
  and the `bucketSessions`/`sessionLabel` helpers; the separate
  `SessionList`/`SessionDetail` and `MobileSessionList`/`MobileSessionDetail`
  components exist only for the `useMediaQuery` mount gate, which prevents two
  concurrent `StreamTranscript` subscriptions — see `adr/0042`.
- **Committing Discord/GitHub/Anthropic tokens** to this repo or any
  target repo, in code, manifests, or CI config.
- **A bespoke per-app Helm chart.** Always reuse
  `infra-bootstrap/gitops/platform/common-app-chart` — see `adr/0026`.
- **A bare streaming-watch RPC without a durable resume cursor for the
  transcript.** Pull/cursor reads only — see `adr/0013`
  (successor to the same message-loss concern `adr/0001` raised about
  Redis pub/sub, now against a gRPC/Postgres backend instead of Redis).
- **A hand-rolled `CREATE TABLE`/partial-schema test fixture, or a second
  copy of the schema anywhere outside `db/migrations/`.** This exact
  pattern caused two separate live incidents (a missing `guidance` column,
  then a missing `suggested_permission_mode` column that shipped
  undetected because the copy that mattered was never updated). Every
  integration test uses `core/internal/dbtest.NewPool(t)`, which applies
  the real `db/migrations/` via golang-migrate — see `adr/0030`.
- **An orchestration framework (Hermes, OpenClaw, or similar).** A single
  Agent SDK planning session, using real Claude Code skills
  (doubt-driven-development, architecture-interview) for structured
  review/elicitation instead of a second independent session or an
  external orchestrator — see `adr/0002` (superseded) and `adr/0017`.
- **A new passthrough RPC on `core` for a service that needs no `core`
  state.** If `core` would open no connection, write no row and resolve
  nothing, it does not belong in the path — add a `ServiceEndpoint` roster
  entry and a NetworkPolicy rule instead. This is the cost that produced
  `adr/0035`'s named exception; `adr/0045` removes the cost rather than the
  rule. A *field* passed through an existing response is fine — a *call*
  relayed through a new one is not.
- **Fleet-managed `infra-bootstrap` cluster ops.** That repo's own
  `CLAUDE.md` states a human runs kubespray/ansible/pigsty personally;
  this fleet does not touch that, full stop, until that decision is
  explicitly revisited in `infra-bootstrap` itself.

Don't propose any of these without an explicit greenlight from Mohammad,
even as a "better alternative" — each one was tried, considered, or hit a
live incident that's recorded in `adr/`.
