# agent-fleet

A self-hosted fleet of Claude Code workers: each owns one feature end-to-end
(plan → code → tests → docs), driven from a web console, running on
`ukubi-cluster`.

**`docs/ARCHITECTURE.md` is canonical for topology/current features;
`docs/DECISIONS.md` + `docs/adr/` are canonical for decisions/rationale.**
This file is a condensed summary for quick orientation — if they disagree,
`docs/ARCHITECTURE.md` wins for specs, `docs/DECISIONS.md`/`docs/adr/` win
for decisions. When in doubt, open those files.

`VISION.md` sits above all of them and is subordinate to all of them: it
states what the fleet is *for*, and audits which of the organism's
principles are already structural here versus still only asked for
(`infra-bootstrap/VISION.md` is the parent). Read it when deciding
*whether* to build something; read the others when deciding *what*.

This repo is submoduled into
[`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap) at
`agent-fleet/` — that repo owns the cluster (Kubernetes, ArgoCD, ingress,
secrets backend); this repo owns the fleet's own source and deploy config.

As of `docs/adr/0019`–`0021`: `fleet-core` → `core`, `e2e-provisioner` →
`provisioner`, plus a new `sidecar` component and a rewritten single-shot
`worker/`. There is no per-repo persistent worker pod anymore — see below.
As of `docs/adr/0029`: a **session** (not a task) is the durable unit — a
worker pod is ephemeral compute attached to a session on demand ("warm"),
and `canUseTool` reproduces the Agent SDK's own live permission-prompt
tiers instead of a fleet-imposed Write/Edit approval gate.

As of `docs/adr/0048` (v3.0.0), that rename is literal and a lot is simply
gone: `tasks` → `sessions` on the wire and in the schema, no queue/lease/
retry/status, no worktrees (a PVC per session), and no sandbox pod — it
supersedes eight earlier ADRs outright. Liveness reconciles against
Kubernetes every 60s rather than against a heartbeat. **If something in this
file or in `docs/` describes a queue, a status enum, a worktree or an e2e
sandbox, it is stale — check the code.**

## Tech stack

| Layer | Tool |
|---|---|
| Runtime (`worker/` only) | Bun (`oven/bun:1-slim`), TypeScript, no build step — Bun runs `.ts` sources directly. The sole remaining JS runtime in the fleet — it's the only process hosting the Agent SDK |
| Worker agent runtime | `@anthropic-ai/claude-agent-sdk` `query()` in **streaming-input mode** (not `claude -p` headless, not the plain-string form) — one continuous session spans planning and implementation, starts in `"default"` permission mode (CLI parity), model `claude-opus-4-8` (see `docs/adr/0021`/`0029`) |
| Runtime (`core/`, `provisioner/`, `sidecar/`) | Go, `go.work` workspace, `golangci-lint` |
| Auth | **authentik OIDC, terminated inside `core`** (`docs/adr/0056`) — `go-oidc`/`oauth2`, state+nonce+PKCE, a `__Host-`prefixed signed cookie, a required `platform-admins` claim, and fail-closed startup. NOT a Traefik forwardAuth middleware: that gates the ingress and cannot see a pod-network caller, which was `#200`. `CoreService` (9090) authenticates separately, with the session's own `lease_id` (`docs/adr/0057`). The e2e previews DO use forwardAuth — no fleet code is in their request path |
| Discord | `discordgo` (Go, in `core/`) — **outbound only**: it posts when a session needs a human and links to the dashboard. The slash commands, threads and relayed replies are gone (`docs/adr/0048`), because Discord has no authorization model and an interactive control there would let anyone in the channel approve a Bash on any session |
| Coordination | Postgres `transcript` (renamed from `planning_transcript`) — pull/cursor reads via `core`'s gRPC `CoreService`, real idempotency-keyed dedup, not pub/sub (see `docs/adr/0013`/`0020`) |
| MCP | `mark3labs/mcp-go` HTTP server (Go, `sidecar/` — one per worker pod, agent connects over `localhost`). Local only: the sandbox the sidecar used to dial is merged into the worker pod (`docs/adr/0048` §6), so nothing crosses a pod boundary over MCP any more |
| gRPC/proto | `buf`-managed `.proto` schema (`proto/`) — the only inter-process/inter-pod protocol in the fleet: `CoreService` (core's server — sidecar + provisioner call it), `ProvisionerService` (provisioner's server — only core calls it), plus the dashboard's ConnectRPC API (`core` ↔ `dashboard/`, `connect-go`/`connect-web`, see `adr/0015`) |
| Kubernetes client | `client-go` (`provisioner/` — the only fleet component with cluster RBAC) |
| Database | `jackc/pgx/v5` (`core/` only — the fleet's **sole** `AGENTFLEET_DB_*` credential holder, `docs/adr/0020` point 1). `worker/`/`sidecar/`/`provisioner/` hold zero DB credentials |
| Deploy | Docker (`worker/Dockerfile`, `core/Dockerfile`, `provisioner/Dockerfile`, `sidecar/Dockerfile`), `core` via a two-source ArgoCD Application (chart from `infra-bootstrap`, values from `k8s/core.yaml` here); `provisioner` as a standalone plain-manifest Application (`k8s/provisioner/` here, RBAC `common-app-chart` can't express) |
| CI/CD | GitHub Actions: `docker.yml` (build/push/deploy), `go.yml` (Go vet/lint/test + buf lint/breaking/drift), `release.yml` (`release-it`, conventional-changelog). **Images build on ukubi-cluster's `build-runner` LXC with `buildah` and push to the in-cluster Zot registry `registry.bnei.lan:5000`, not Docker Hub** — one job looping all six components, since a self-hosted runner runs one job at a time (infra-bootstrap ADR-0034) |

## Directory map

| Path | What |
|---|---|
| `docs/ARCHITECTURE.md` | Canonical topology + current features (the WHAT) |
| `docs/DECISIONS.md` | Canonical settled decisions, forbidden patterns (the WHY, short form) |
| `docs/adr/` | One Architecture Decision Record per real decision |
| `core/` | Go — Discord notifications + `CoreService` gRPC server + transcript coordination + Loki/Prometheus queries + the web dashboard's ConnectRPC API and static SPA, one binary, **no cluster RBAC**, sole Postgres-credential holder. No dispatch loop and no queue (`docs/adr/0048`): a message provisions, and a 60s reconcile loop (`internal/sessions/reconcile.go`) writes `pod_phase` from what Kubernetes actually reports, then runs the stop-grace, startup-stall, idle and retention sweeps off it |
| `provisioner/` | Go — the **only** fleet component with cluster RBAC over the fleet's own namespace. Owns all pod creation and the git lifecycle. **No worktrees** (`docs/adr/0048` §5): each session gets its own PVC holding a real clone, and the shared PVC keeps a read-only repo cache plus per-session `claude-home`. Zero DB credentials — Kubernetes itself is its source of truth |
| `sidecar/` | Go — new second container in every worker pod. Local MCP server (agent-facing) + local plain HTTP/JSON API (wrapper-facing, including the live human-message SSE feed) + an independent telemetry loop, all funneled through one outbound gRPC connection to `core` |
| `dashboard/` | React + Vite + TypeScript + Tailwind/DaisyUI SPA, talks to `core` via a generated ConnectRPC client, built into `core`'s binary — not deployed on its own. Rewritten as a console in `docs/adr/0042`: four full-width nav views (**sessions**, **audits**, **files**, **observability**) plus the session detail that replaces the list rather than sitting beside it; **observability** is a live fleet topology + PromQL explorer (`docs/adr/0047`); decisions answerable **from the list**; a five-tier feed with one three-way DENSITY control; one pinned `DecisionDock` on both form factors (`docs/adr/0043` — the desktop spine is gone; `DENSITY → decisions` is the zoom-out); two themes; and an installable PWA whose service worker deliberately caches **no** fleet state (a cached session list would have someone answering a resolved decision). URL state is `?view=`/`?session=`, with the pre-rename `?task=` still read. Desktop and mobile share `SessionFeed`/`SessionPanels`/`DecisionInline`/`DecisionDock`, plus `bucketSessions` and `sessionLabel` — put anything both surfaces need there, not in one of them. `MermaidDiagram` is `React.lazy`-loaded, deliberately: mermaid is the largest thing this app ships and almost nothing renders a diagram |
| `worker/` | The Claude Code worker (TS/Bun; `src/session.ts`, renamed from `planning.ts`) — **single-shot**: one pod per warm, one continuous streaming-input session spanning planning+implementation (resumable via `RESUME_SESSION_ID`), then exits. Talks only to its own pod's `localhost` sidecar |
| `executor/` | Go — thot-executor plus the `kubectl-shim` installed into a cluster-access session as `kubectl`. The fleet's **only** process holding cluster RBAC on behalf of an agent (`docs/adr/0037`), so a thot session stays an ordinary worker pod with zero Kubernetes credentials. Its identity comes from a ServiceAccount defined in `infra-bootstrap`'s gitops, never named by the provisioner |
| `fleet-shared/` | The git-tracked source for every worker pod's shared Claude Code context — `settings.json`, `skills/`, `CLAUDE.md`. The provisioner clones it and `SyncFleetShared` lays it into each session's own `claude-home` (`docs/adr/0032`). It is the `"user"` half of `settingSources`; the `"project"` half is the target repo's own `CLAUDE.md`/`.claude/skills/`. What stops that repo's `permissions.allow` widening the fleet's authority is **not** in this directory any more: it is `FLEET_ASK_RULES` in `worker/src/session.ts`, injected per-session through the SDK's `settings` option (`docs/adr/0052`, moving `adr/0049`'s original `permissions.ask` block out of `settings.json`). What an ask *means* is decided in `canUseTool`, not by the rule: in `auto` the worker answers everything but `rm`/`sudo` itself (`docs/adr/0053`) |
| `proto/` | buf-managed `.proto` schema: `CoreService`, `ProvisionerService`, `DashboardService` — shared by `core`/`provisioner` (Go codegen) and `worker`/`dashboard` (TS codegen) |
| `db/migrations/` | Sole source of truth for the `sessions`/`proposals`/`transcript`/`repos`/`prompt_snippets`/`scheduled_audits`/`knowledge_journal` schema (`agentfleetdb`), applied via golang-migrate — see `docs/adr/0030` |
| `local/kind/` | The disposable kind cluster the `/kind-local` skill stands up to exercise real pod dispatch without touching `ukubi-cluster` |
| `migration/` | The image that applies `db/migrations/` (`FROM migrate/migrate:latest`) — run as `k8s/core.yaml`'s `hooks.migrate` PreSync job, not part of `core` itself |
| `k8s/` | `core.yaml` (Helm values, `common-app-chart`) + `provisioner/` (standalone plain manifests: Deployment/Service/ServiceAccount/Role/InfisicalSecret/NetworkPolicy/PVC) |

## Locked decisions (condensed — full detail + rationale in `docs/DECISIONS.md` and `docs/adr/`)

- Session coordination is a durable Postgres table (`transcript`, renamed
  from `planning_transcript`), read via a pull/cursor API, never a bare
  streaming-watch RPC without a resume cursor — a dropped message during a
  live permission decision isn't recoverable (see `docs/adr/0013`,
  successor to the original Redis-list-over-pubsub decision).
- No orchestration framework (Hermes/OpenClaw rejected) — a single Agent
  SDK session, using real Claude Code skills (doubt-driven-development,
  architecture-interview) for review/elicitation instead of a second
  independent session — see `docs/adr/0017`.
- **One pod per warm, single-shot, spawned on demand by the provisioner —
  not one persistent pod per target repo, and not tied to a session's
  entire lifetime.** A pod is ephemeral compute attached to a session
  on-demand (`Warm`, or a fresh dispatch), torn down on Stop or idle
  timeout, resumable later via the session's saved `session_id`
  (`docs/adr/0029`). Fleet-wide concurrency (default cap 5, reinterpreted
  as "max warm pods"), not one-session-per-repo. Isolation is a **PVC per
  session** holding a real clone — not a worktree, and not a directory inside
  a shared writable volume (`docs/adr/0048` §4/§5).
- **Postgres access is fully centralized in `core`.** No other
  component — provisioner, worker, or sidecar — ever holds
  `AGENTFLEET_DB_*` credentials (`docs/adr/0020` point 1).
- **`core` commands, the provisioner executes — never the reverse.** The
  provisioner never decides to spawn a pod on its own (`docs/adr/0020`
  point 2). There is nothing to claim: the queue is gone.
- **Hub-and-spoke for *lifecycle*: nothing commands the provisioner except
  `core`.** Pod creation and teardown route agent → sidecar → `core` →
  provisioner (`docs/adr/0020` point 4).
- **MCP is local, full stop** (`docs/adr/0048` §6). The sandbox the sidecar
  used to dial is merged into the worker pod, so no MCP call crosses a pod
  boundary and the provisioner does not speak MCP at all. gRPC remains the
  only protocol between fleet components.
- **No prospective Write/Edit gate — `canUseTool` reproduces the Agent
  SDK's own live permission prompt instead** (`docs/adr/0029`, supersedes
  `docs/adr/0021`'s `allowedTools`-absent + in-memory-`approved`-flag
  mechanism). Sessions start in `"default"` mode (CLI parity); the SDK's
  own `permissionMode` decides when `canUseTool` gets invoked, and
  `canUseTool` just asks a human and blocks — never inferred from silence
  or round completion. A permission decision is always a real, structured
  `RespondToPermission` (or `SetPermissionMode`) call. `/approve` no
  longer exists.
- **The gate is `canUseTool`, not a rule list the SDK re-interprets**
  (`docs/adr/0053`). Two answers are the fleet's own and never reach a
  human: its **own** MCP tools (`mcp__agent-fleet-sidecar__*`,
  `mcp__playwright__*`) are always allowed — otherwise the agent needs
  permission before it can *ask* for permission, which is what shipped —
  and in `auto` everything is allowed except a `Bash` running `rm`/`sudo`.
  `allowedTools` still lists the MCP tools but nothing depends on it:
  0.3.233 asks for every non-read-only MCP tool in `plan` mode, and again
  under an org `effectiveMaxPermission` ceiling, both **above** the
  allow-rule lookup. `bypassPermissions` is deleted — it bought only
  `rm`/`sudo` over `auto` and charged a launch profile for them.
- **There is no sandbox** (`docs/adr/0048` §6, superseding `docs/adr/0039`/
  `0044`/`0045` entirely). Builds, tests and installs run in the worker pod's
  own `Bash`, gated by `canUseTool` like everything else — `run_command`,
  `request_e2e_env` and `kill_env` are all deleted. Three consecutive ADRs
  went into repairing a second pod whose entire purpose was letting **one
  tool** skip a permission prompt; the fix was deleting the pod, not the
  fourth repair.
- **A session with no message has no pod, and that is the human gate**
  (`docs/adr/0048` §1). `CreateSession` writes a row and nothing else;
  the first message provisions. Ordering is **warm-then-append, never the
  reverse** — `resumeFromSeq` is `LatestSeq` computed at dispatch, so a
  message appended before the pod exists lands below its cursor and is never
  delivered. `PostMessage`/`OpenFromProposal`/`PromptSession` all carry the
  comment; copy it rather than re-deriving it.
- **The dashboard is a console, and the list answers decisions** — a blocked
  session renders its actual pending permission/question inline, answerable
  without opening it, because a blocked session is stalled until a human clicks
  (`docs/adr/0042`). The feed's five tiers must stay visually distinct; a new
  entry kind belongs in a tier, not in a uniform grey log line. Anything both
  form factors show goes in the shared components, never one of them.
- **The console's gate lives in `core`, not at the ingress** (`docs/adr/0056`).
  `fleet.bnei.dev` has exactly one lock now — `basic-admin-auth` is gone — and
  the IngressRoute matches on host with no path constraint, so **a new exempt
  path is a new public endpoint**. Unset OIDC config makes core refuse to start;
  `FLEET_AUTH_DISABLED=1` is the explicit local-stack opt-out.
- **Nothing on `CoreService` is callable without the session's `lease_id`**
  (`docs/adr/0057`), and authorization is an explicit per-method table — a
  field-name rule is wrong on `PromptSession`, `SaveAgentSessionId` and
  `GetSession`, each time in the direction that grants authority. A method in no
  table fails closed.
- **`X-authentik-*` headers are never read.** A pod-network caller can forge
  one, which would turn an authorization gap into an impersonation one. core
  verifies its own ID token instead.
- Every result is a PR.
- One PVC per session — never a shared writable repo checkout across
  concurrent sessions. The fleet does not create branches either: the agent
  runs its own `git checkout -b` (`docs/adr/0048` §5).
- Git commit identity is derived live from the authenticated bot GitHub
  account (`gh api user`), never hardcoded — done independently by both
  the provisioner (clone/fetch) and the worker (push/PR), since they're
  separate pods that don't share `$HOME`.
- Claude Code auths via subscription OAuth (`CLAUDE_CODE_OAUTH_TOKEN`), not
  a metered API key — no cost cap by design.
- Secrets only via Infisical (project `agent-fleet-nygh`) — never
  committed.

## Forbidden patterns (quick check — full list + reasons in `docs/DECISIONS.md`)

A shared writable repo volume across sessions · inferring a permission
decision from silence · appending a message before the pod that must read it
exists · sending a human's instruction as `description` (the agent never reads
it) · hardcoded git commit identity · committing Discord/GitHub/
Anthropic tokens · a bespoke per-app Helm chart (reuse
`infra-bootstrap/gitops/platform/common-app-chart`) · any component other
than `core` holding `AGENTFLEET_DB_*` credentials · a worker/sidecar
*commanding* the provisioner directly (pod lifecycle must route through
`core`) · a second pod to let one tool skip a permission prompt
(`docs/adr/0048` §6) · reintroducing a status enum, a queue, a lease-renewal
heartbeat or a retry counter (`docs/adr/0048` §2) · fleet-managed
`infra-bootstrap` cluster ops (kubespray/ansible/
pigsty — that's the parent repo's own `CLAUDE.md` boundary, explicitly
blocked here too).

## Current targets

`dream-analyst`, `vos-monolith`, `agent-fleet` — real repos, seeded into
the dashboard-editable `repos` table (`docs/adr/0028`) — no redeploy
needed to add/edit one. No per-repo Deployment/PVC — onboarding a new repo
is a "manage repos" entry in the dashboard, not new k8s manifests.

## Verification traps

Green CI is necessary, not sufficient. These failure modes were all *silent* —
the check passed, or wasn't run, or measured the wrong thing. The first five
each shipped a bug on 2026-08-11; the last one had been shipping since the
feature was written:

- **`tsc --noEmit` is NOT the build.** The real command is
  `bun run build` (`tsc -b && vite build`), and `tsc -b` enforces
  `noUnusedLocals`. An unused import passed the weaker check and broke
  the core image. **Verify with `bun run build`.**
- **A PR build never pushes an image.** "The image built" and "the image
  exists" are different claims — a manifest referencing a tag only a PR built
  will `ImagePullBackOff`. Check the registry itself:
  `curl -s http://registry.bnei.lan:5000/v2/agent-fleet-worker/tags/list`
  (anonymous read). And note that the registry keeps only the **last 3 tags**
  per image plus `latest`, so "it was pushed once" is not "it is still there"
  either — a rollback deeper than 3 releases needs a rebuild.
- **`kubectl apply --dry-run=server` does not create a pod.** A Deployment
  naming a non-existent ServiceAccount validates perfectly and then never
  schedules. ArgoCD reporting `Synced` + `Degraded` together is the
  signature — look at pods, not sync status.
- **An unset GitHub secret is the empty string, not an error.** `docker.yml`
  read the registry username from `secrets.REGISTRY_USERNAME`, which was never
  set on this repo — the value had been put in Infisical instead, and a commit
  message asserted the workflow used a plaintext env. So `buildah login` got
  `--username ""` and the v3.8.6 release died on "Must provide --username with
  --password-stdin", 8 seconds in, *after* the tag existed. Nothing warns you:
  `${{ secrets.X }}` for a missing X interpolates empty and the step runs. Same
  family as the `repos.image` trap below — a value present at several layers
  and reaching none of them. **A non-secret that CI needs belongs in `env:`
  where its absence is a YAML-visible fact, not in `secrets:` where it is
  indistinguishable from empty.**
- **Adding a component means wiring it into *every* CI path**, including
  `docker.yml`'s `COMPONENTS` env (the release list) *and* the `changes`
  job's paths-filter + component script (the PR list — a component missing
  only from there builds on releases and never on PRs), plus the codegen
  install steps in `go.yml`. Guarded by `core/internal/buildguard` — run with
  `-count=1`, since those tests read files Go's cache cannot see. Note what
  that guard does and does not prove: it greps `docker.yml` for the directory
  name, so listing a component in `COMPONENTS` alone satisfies it.
- **Squash-merging a stacked PR auto-closes the PR above it** (its base
  branch is deleted, and GitHub refuses to reopen). Retarget dependent
  PRs to `main` *before* merging the one below.
- **A squash also drops any change the stacked branch made and then undid.**
  Both halves of an add-then-delete cancel inside the squashed range, so the
  net diff says nothing about that file — while `main` has meanwhile gained
  it from the lower PR's own squash. No conflict, and the file survives a
  merge that was supposed to remove it. `docs/plan-dashboard-session-model.md`
  shipped to `main` exactly this way: #159 deleted the file #158 added, both
  merged green, and the file was still there. **After merging a stack, `ls`
  the files the upper PR claimed to delete** — the PR diff will not tell you.
- **A bound port is not a reachable service is not a working capability.**
  Browser automation was dead for the fleet's entire history behind three
  stacked failures, and each layer of checking passed the one below it
  (`docs/adr/0044`): the port was *bound* (so `--port` looked verified), but
  the server 403'd every non-localhost `Host`; once reachable, the tool list
  was fetched 3s before the server came up; once registered, the browser
  binary `@playwright/mcp` resolves wasn't installed. Only an actual
  `browser_navigate` found the last one. **When a component's whole job is to
  do something, verify it doing that thing** — not that its process is up,
  its port is open, or its handshake succeeds. Applies to anything reached
  through the agent → sidecar → core → provisioner → pod proxy chain, where a
  failure at any hop is swallowed into an empty result.
- **A wide rename can silently swap two same-typed fields.** `task_id` →
  `session_id` (docs/adr/0048) left `SaveAgentSessionId` passing
  `req.GetSessionId()` for *both* the row key and the SDK's own conversation
  id. Both are strings, so it compiled, linted, and passed every mocked unit
  test — and every resume silently began a brand-new conversation, because
  the SDK was handed an id that was never one of its own. The only symptom is
  "the warmed agent doesn't remember anything." **After renaming a field
  across a proto, check the call sites where the old and new names now
  collide**, not just that it builds. Guarded by
  `TestSaveAgentSessionId_StoresTheAgentIdNotTheSessionId`.
- **`process.exitCode = 1` is not an exit.** It sets the code and waits for
  the event loop to drain; one lingering handle and the worker runs forever
  with the right exit code and no exit. From outside that is indistinguishable
  from a working session — the Job never reaches a terminal phase, `pod_phase`
  stays RUNNING, and the slot is never released. In a single-shot process,
  exit explicitly. Same failure class as the `Succeeded`-Job gap below.
- **The fleet wedges silently after `MAX_LIVE_SESSIONS`, and only then.**
  Anything that leaves a finished session non-terminal — an unreported
  `Succeeded` Job, a pod that never exits, an archived row still counted —
  costs one permanent slot. Nothing errors until the sixth session, which is
  why the check is "run six sessions and open a seventh", not "run one and
  see that it works."
- **A nullable column read into a plain `string` fails the whole query, not
  the row.** `sessions.description` is nullable and `Session.Description` is a
  `string`, so one NULL row broke `scanSession` — and `List` returns a single
  error, so that row emptied the session list, the reconcile loop's view of
  the fleet, and the live-state gauge at once. It went unnoticed because
  `Create` writes `''`: "every writer passes a non-NULL" was a convention, not
  a constraint, and `docs/adr/0048` had just made `description` a vestigial
  label nothing sets on purpose. **A nullable column needs a nullable scan or
  a `COALESCE`, whatever today's writers happen to do.** Guarded by
  `TestList_SurvivesANullDescription`.
- **A whole feature can be dead without one test noticing, if nothing drives
  the UI.** Creating a session from the dashboard did nothing for the whole of
  v3.0.0 — the dialog sent an instruction as `description`, a column the agent
  never reads, so no pod booted and nothing errored. Every Go test, every
  `bun test`, and `bun run build` passed throughout; `/dashboard-e2e` found it
  in one click, and found a second live bug (the NULL scan above) while
  setting up. **Run `/dashboard-e2e` after changing anything in the create,
  decision or warm path**, not just the unit tests.
- **A config value can exist at every layer and still reach nothing.**
  `repos.image` shipped with `docs/adr/0048` as a column, a store round-trip,
  a proto message on the dashboard side and a labelled input — and no `image`
  field on `CreateWorkerPodRequest`, so it never crossed core→provisioner and
  `pod.go` used `WORKER_IMAGE` regardless. Editing it in "manage repos" did
  nothing for three minor versions. Grepping the field name finds hits in
  every layer and looks like a wired feature; the four toolchain ingredients
  it was supposed to replace were still shipping alongside it, one of them
  still shadowing the image's own Go on `PATH`. **For anything a human
  configures, trace it to the thing that consumes it — a column, a UI input
  and a passing test suite are three ways of describing intent, not
  evidence.** Guarded by
  `TestCreateWorkerPod_RepoImageAppliesToTheWorkerContainerOnly`.
- **Deleting a pod deletes its resource envelope, and the work moves without
  it.** `bc5da8f` deliberately sized the e2e sandbox for building — 250m/512Mi
  → 1000m/1Gi requests, 2Gi → 4Gi limits — on the finding that compiles and
  installs do not fit the smaller one. Six days later `docs/adr/0048` §6
  deleted that pod and moved every build into the worker's own `Bash`, and the
  worker kept the numbers it had when it ran an agent and nothing else. Nothing
  got heavier; the heavy work moved in. The failure is invisible because cgroup
  v2 sets `memory.oom.group` on a container scope: crossing the limit does not
  kill the greedy process, the kernel SIGKILLs **every** task in the container,
  the agent and PID 1 included. So there is no failed tool call, no error line,
  no `session failed` — the logs just stop mid-sentence and the Job goes
  Failed. Prometheus won't show it either: the spike lives ~10s between 30s
  scrapes and staleness-fill makes the series look flat. **When a capability
  moves between pods, move its limits, its requests and its guard test with
  it** — and to confirm an OOM, read the node's `dmesg`, not a dashboard.
  Guarded by `TestCreateWorkerPod_WorkerResources` and
  `TestCreateWorkerPod_ResourcesWithinLimitRange` (the LimitRange pin was
  itself deleted along with the sandbox it guarded).
- **A terminal Job deleted on sight takes the only evidence with it.** The
  provisioner's reconcile pass deleted a Failed worker Job within ~60s — pod
  died 19:24:14, `DeleteWorkerJob` 19:24:22 — so kube-state-metrics never
  scraped a terminated state, `kube_pod_container_status_last_terminated_reason`
  had no worker rows at all, and `kubectl describe` had nothing to describe.
  The Job's own `TTLSecondsAfterFinished` existed for exactly this and had
  never once been allowed to run. **A GC that beats your telemetry to the
  corpse makes every crash look like a disappearance.**
- **`fsGroup` does not change a file's owner, and the fake clientset never runs
  the script it asserts on.** The browser-cache Job died on `chmod -R o+rX
  /browsers` with "Operation not permitted" — after the full 300 MB download,
  every time. The fix added `fsGroup: 1000`, which chgrps the subPath and adds
  `g+w` but leaves it owned by root; the Job runs as uid 1000
  (`worker/Dockerfile` ends `USER bun`), and chmod on a directory you do not own
  is EPERM in any group. The same alert fired again the next day. Both the code
  comment and the test comment asserted the Job ran "as root" — a premise
  nothing had checked, and one `client-go`'s fake clientset structurally cannot
  check, because it validates the manifest and never executes it. Worse, the Job
  then stayed failed forever: `EnsureBrowserCache` compared images and never read
  `.status`, so a `BackoffLimitExceeded` Job was indistinguishable from a healthy
  completed one. **For a manifest whose payload is a shell script, the fake
  clientset proves the shape and nothing about the behavior — read the pod's
  actual log. And a component that reconciles a resource must look at whether it
  failed, not only at whether it exists.** Guarded by the script assertions in
  `TestEnsureBrowserCache_WritesWhatTheWorkersRead` and by
  `TestEnsureBrowserCache_RecreatesAFailedJob`.

- **`MERGEABLE` is not "compiles".** Two PRs added a package import named `auth`
  and a local variable named `auth` to the same function. Each was green on its
  own branch, neither branch contained the other's half, and there was **no
  textual conflict** — so GitHub reported the second one mergeable, the squash
  was clean, and the build broke only at the release, where the variable
  shadowed the package. Git prompts a rebase when it sees overlapping *text*; a
  semantic collision produces no signal at all. **After merging into a branch
  that has moved, build the merge result, not the branch** — and treat "no
  conflict" as saying nothing about whether the two halves agree. Cost here was
  bounded only by pipeline shape: `build-push` failed, so `deploy` was skipped
  and no image tag moved.
- **An OIDC issuer is compared byte-for-byte, so normalising it is a change.**
  `auth.New` passed the configured issuer through `strings.TrimSuffix(_, "/")`
  as tidying; authentik's issuer ends in a slash, so discovery rejected it and
  core crash-looped — **a 15-minute console outage**, because that path fails
  closed on purpose and a config-shaped bug there is an outage rather than a
  degraded login. `k8s/core.yaml`'s own comment said "the trailing slash
  matters" one file away. Nothing caught it because **no test exercised real
  discovery**: every auth test drove the signer, the gate and the cookie, and
  the path from config value to provider was unobserved until it met authentik.
  Guarded by `TestNew_PassesTheIssuerThroughUnchanged` and
  `TestNew_DoesNotNormaliseTheIssuer`, against an httptest stub shaped like
  authentik's — issuer with a trailing slash, `userinfo` deliberately **not**
  under it.
- **`fleet.bnei.dev` now has exactly one lock, and the IngressRoute matches on
  host with no path constraint.** So every path core serves on 8080 is public
  unless the in-app gate refuses it, and **a new exempt path is a new public
  endpoint**. Currently exempt: `/healthz`, `/auth/*`, `/webhook/alertmanager`
  (its own bearer token, refuses when unset). `/metrics` is deliberately not.
  There is no basic-auth behind it any more and core has no local admin, so an
  authentik/Pigsty/Patroni outage means no console — recovery is
  `FLEET_AUTH_DISABLED=1` plus a redeploy, or `kubectl port-forward`.
- **"Zero rejections in the logs" is not "the auth works" when there was no
  traffic.** After deploying `CoreService` lease auth, core showed zero auth
  failures — and also zero `CoreService` calls and zero worker pods in the same
  window. The number measured absence of traffic. For anything gated, the check
  is a call that *should* be refused actually being refused: from a worker pod,
  `CoreService` with **another session's** valid lease.

## Workflow rules

- All changes via feature branch + PR. No direct push to `main`.
- Secrets never committed; always fetched from Infisical at run time.
- Commit messages follow Conventional Commits — `release.yml` runs
  `release-it` off them to bump version/CHANGELOG on every push to `main`.
- `worker/` has `bun:test` coverage (`bun run test`) for coordination logic
  that's cheap to mock — the SDK's streaming-input `query()` and the
  sidecar's local HTTP client — run in CI ahead of the Docker build; also
  has `bun run typecheck`.
- `core/`, `provisioner/`, and `sidecar/` have `go test` coverage:
  table-driven unit tests, `client-go`'s fake clientset for k8s manifest
  shape, `bufconn` for gRPC roundtrips, and `testcontainers-go`-backed
  integration tests (gated `-tags=integration`) against a real Postgres —
  see `.github/workflows/go.yml`. `golangci-lint` runs alongside.
- The Postgres schema's sole source of truth is `db/migrations/` — a
  schema change is always a new numbered `.up.sql`/`.down.sql` pair,
  applied via golang-migrate (see `migration/Dockerfile`), never an edit
  to an existing migration file, a hand-rolled `CREATE TABLE` test
  fixture, or any other copy of the schema. Integration tests use
  `core/internal/dbtest.NewPool(t)`. This is CI-enforced by construction
  (one real source, not a diff-check) after two separate incidents where a
  hand-copied schema shipped without a column — see `docs/adr/0030`.

## Skills

- `/fleet-ops` — onboard a new target repo, walk the release/deploy
  pipeline, inspect what is live right now.
- `/fleet-feature` — checklist for adding functionality to `core/`,
  `provisioner/`, `sidecar/`, or `worker/` (new gRPC method, new MCP tool,
  new slash command, new transcript entry type).
- `/fleet-debug` — diagnose a stuck or failed session (session/journal
  state, worker/sidecar/provisioner logs, the `transcript` table, known
  failure modes).
- `/dashboard-e2e` — spin up the minimal local stack (throwaway Postgres +
  stub provisioner + `core` + dashboard dev server) to Playwright-test the
  dashboard UI, then tear it all down. **Run it after touching the create,
  decision or warm path** — see the last two Verification traps.
- `/kind-local` — a disposable kind cluster running the real fleet, for the
  pod-dispatch path `/dashboard-e2e`'s stub deliberately fakes.
