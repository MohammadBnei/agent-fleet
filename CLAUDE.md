# agent-fleet

Self-hosted fleet of Claude Code workers. One feature each, end to end (plan →
code → tests → docs). Driven from web console. Runs on `ukubi-cluster`.

**Canonical:** `agent-fleet/docs/ARCHITECTURE.md` = topology/features.
`agent-fleet/docs/DECISIONS.md` + `agent-fleet/docs/adr/` = decisions/rationale.
This file = condensed orientation only. Disagreement → those win.

Paths are repo-qualified (`agent-fleet/...`) because this repo is a submodule of
`infra-bootstrap` — bare `docs/` is ambiguous from the parent. Inline `adr/NNNN`
is shorthand for `agent-fleet/docs/adr/NNNN`.

`VISION.md` above all, subordinate to all: what fleet is *for*, which organism
principles structural vs still aspirational. Parent = `infra-bootstrap/VISION.md`.
Read it deciding *whether* to build. Read others deciding *what*.

Submoduled into [`infra-bootstrap`](https://github.com/MohammadBnei/infra-bootstrap)
at `agent-fleet/`. That repo owns cluster (k8s, ArgoCD, ingress, secrets backend);
this one owns fleet source + deploy config.

**Stale-doc warning.** `adr/0019`–`0021`: `fleet-core`→`core`, `e2e-provisioner`→
`provisioner`, new `sidecar`, rewritten single-shot `worker/`. `adr/0029`: session
(not task) = durable unit; pod = ephemeral compute attached on demand ("warm");
`canUseTool` reproduces SDK's own permission tiers. `adr/0048` (v3.0.0) made rename
literal + deleted a lot: `tasks`→`sessions` on wire and in schema, no queue/lease/
retry/status, no worktrees (PVC per session), no sandbox pod. Supersedes eight ADRs.
**Anything here or in `docs/` describing queue, status enum, worktree or e2e sandbox
is stale → check code.**

## Tech stack

| Layer | Tool |
|---|---|
| Runtime (`worker/` only) | Bun (`oven/bun:1-slim`), TS, no build step — Bun runs `.ts` direct. Only JS runtime left; only process hosting Agent SDK |
| Worker agent runtime | `@anthropic-ai/claude-agent-sdk` `query()`, **streaming-input mode** (not `claude -p`, not plain-string). One session spans plan+impl. Starts `"default"` mode (CLI parity), model `claude-opus-4-8` (`adr/0021`/`0029`) |
| Runtime (`core/`, `provisioner/`, `sidecar/`) | Go, `go.work`, `golangci-lint` |
| Auth | **authentik OIDC terminated inside `core`** (`adr/0056`) — `go-oidc`/`oauth2`, state+nonce+PKCE, `__Host-` signed cookie, required `platform-admins` claim, fail-closed startup. NOT Traefik forwardAuth: that gates ingress, can't see pod-network caller → `#200`. `CoreService` (9090) authenticates separately via session's `lease_id` (`adr/0057`). e2e previews DO use forwardAuth — no fleet code in their path |
| Discord | `discordgo` (Go, `core/`) — **outbound only**. Posts when session needs human, links dashboard. Slash commands/threads/replies gone (`adr/0048`): Discord has no authz model → anyone in channel could approve a Bash |
| Coordination | Postgres `transcript` (was `planning_transcript`). Pull/cursor via `core` gRPC `CoreService`. Idempotency-keyed dedup, not pub/sub (`adr/0013`/`0020`) |
| MCP | `mark3labs/mcp-go` HTTP (Go, `sidecar/`) — one per worker pod, agent dials `localhost`. Local only: sandbox merged into worker pod (`adr/0048` §6) → no MCP crosses pod boundary |
| gRPC/proto | `buf`-managed `proto/`. Only inter-process protocol: `CoreService` (core serves; sidecar+provisioner call), `ProvisionerService` (provisioner serves; only core calls), dashboard ConnectRPC API (`core` ↔ `dashboard/`, `connect-go`/`connect-web`, `adr/0015`) |
| k8s client | `client-go` (`provisioner/` — only component with cluster RBAC) |
| DB | `jackc/pgx/v5` (`core/` only — **sole** `AGENTFLEET_DB_*` holder, `adr/0020` pt1). worker/sidecar/provisioner hold zero DB creds |
| Deploy | Docker per component. `core` = two-source ArgoCD app (chart from `infra-bootstrap`, values `k8s/core.yaml`). `provisioner` = standalone plain manifests (`k8s/provisioner/`, RBAC `common-app-chart` can't express) |
| CI/CD | Actions: `docker.yml` (build/push/deploy), `go.yml` (vet/lint/test + buf lint/breaking/drift), `release.yml` (`release-it`). **Images build on ukubi-cluster `build-runner` LXC via `buildah` → in-cluster Zot `registry.bnei.lan:5000`, not Docker Hub.** One job loops all six components — self-hosted runner does one job at a time (infra-bootstrap ADR-0034) |

## Directory map

| Path | What |
|---|---|
| `docs/ARCHITECTURE.md` | Canonical topology + features (the WHAT) |
| `docs/DECISIONS.md` | Canonical settled decisions, forbidden patterns (the WHY, short) |
| `docs/adr/` | One ADR per real decision |
| `docs/verification-traps.md` | Full account of every silent-failure trap. One-liners below |
| `core/` | Go, one binary. Discord notify, `CoreService` gRPC, transcript coordination, Loki/Prometheus queries, dashboard ConnectRPC API, static SPA. **No cluster RBAC.** Sole Postgres-cred holder. No dispatch loop, no queue (`adr/0048`): message provisions. 60s reconcile (`internal/sessions/reconcile.go`) writes `pod_phase` from what k8s reports → stop-grace/startup-stall/idle/retention sweeps run off it |
| `provisioner/` | Go. **Only** component with cluster RBAC over fleet namespace. Owns pod creation + git lifecycle. **No worktrees** (`adr/0048` §5): PVC per session holding real clone; shared PVC keeps read-only repo cache + per-session `claude-home`. Zero DB creds — k8s is its source of truth |
| `sidecar/` | Go. Second container in every worker pod. Local MCP server (agent-facing) + local HTTP/JSON API (wrapper-facing, incl. human-message SSE feed) + telemetry loop. All funneled through one outbound gRPC to `core` |
| `dashboard/` | React + Vite + TS + Tailwind/DaisyUI SPA. ConnectRPC client, built into `core` binary — never deployed alone. Console rewrite (`adr/0042`): 4 full-width views (sessions, audits, files, observability) + detail replacing list; observability = live topology + PromQL explorer (`adr/0047`); decisions answerable **from the list**; 5-tier feed + one 3-way DENSITY control; one pinned `DecisionDock` both form factors (`adr/0043` — desktop spine gone, `DENSITY → decisions` is zoom-out); 2 themes. PWA SW caches **no** fleet state (cached session list → someone answers a resolved decision). URL state `?view=`/`?session=`; pre-rename `?task=` still read. Shared both form factors: `SessionFeed`/`SessionPanels`/`DecisionInline`/`DecisionDock`, `bucketSessions`, `sessionLabel` — put anything both need there. `MermaidDiagram` `React.lazy` deliberately: biggest thing shipped, almost nothing renders a diagram |
| `worker/` | Claude Code worker (TS/Bun; `src/session.ts`, was `planning.ts`). **Single-shot**: one pod per warm, one streaming-input session spanning plan+impl (resumable via `RESUME_SESSION_ID`), then exits. Talks only to own pod's `localhost` sidecar |
| `executor/` | Go. thot-executor + `kubectl-shim`, installed into a cluster-access session as `kubectl`. Fleet's **only** process holding cluster RBAC for an agent (`adr/0037`) → a thot session stays an ordinary worker pod with zero k8s creds. Identity from a ServiceAccount in `infra-bootstrap` gitops, never named by the provisioner |
| `fleet-shared/` | Git-tracked source of every worker pod's shared Claude Code context: `settings.json`, `skills/`, `CLAUDE.md`. Provisioner clones; `SyncFleetShared` lays it into each session's `claude-home` (`adr/0032`). `"user"` half of `settingSources`; `"project"` half = target repo's own `CLAUDE.md`/`.claude/skills/`. What stops that repo's `permissions.allow` widening fleet authority is **not here** — it is `FLEET_ASK_RULES` in `worker/src/session.ts`, injected per-session via SDK `settings` (`adr/0052`, moving `adr/0049`'s `permissions.ask` out of this file). What an ask *means* is decided in `canUseTool`, not the rule: in `auto`, worker answers all but `rm`/`sudo` (`adr/0053`) |
| `proto/` | buf-managed schema: `CoreService`, `ProvisionerService`, `DashboardService`. Go codegen for core/provisioner; TS for worker/dashboard |
| `db/migrations/` | Sole source of truth for `sessions`/`proposals`/`transcript`/`repos`/`prompt_snippets`/`scheduled_audits`/`knowledge_journal` schema (`agentfleetdb`). golang-migrate (`adr/0030`) |
| `local/kind/` | Disposable kind cluster `/kind-local` stands up to exercise real pod dispatch off `ukubi-cluster` |
| `migration/` | Image applying `db/migrations/` (`FROM migrate/migrate`). Runs as `k8s/core.yaml` `hooks.migrate` PreSync job, not part of `core` |
| `k8s/` | `core.yaml` (Helm values, `common-app-chart`) + `provisioner/` (plain manifests: Deployment/Service/SA/Role/InfisicalSecret/NetworkPolicy/PVC) |

## Locked decisions

Condensed. Full detail + rationale in `agent-fleet/docs/DECISIONS.md` + `docs/adr/`.

- Session coordination = durable Postgres `transcript`, read via pull/cursor API.
  Never bare streaming-watch RPC without resume cursor — dropped message during
  live permission decision is unrecoverable (`adr/0013`, successor to the
  Redis-list-over-pubsub decision).
- No orchestration framework (Hermes/OpenClaw rejected). Single Agent SDK session
  using real Claude Code skills (doubt-driven-development, architecture-interview)
  for review/elicitation, not a second session (`adr/0017`).
- **One pod per warm, single-shot, spawned on demand by provisioner.** Not one
  persistent pod per repo, not tied to session lifetime. Pod = ephemeral compute
  attached on demand (Warm, or fresh dispatch), torn down on Stop or idle timeout,
  resumable via saved `session_id` (`adr/0029`). Fleet-wide concurrency (cap 5 =
  max warm pods), not one-session-per-repo. Isolation = **PVC per session** holding
  real clone — not a worktree, not a dir in a shared writable volume (`adr/0048` §4/§5).
- **Postgres access fully centralized in `core`.** No other component ever holds
  `AGENTFLEET_DB_*` (`adr/0020` pt1).
- **`core` commands, provisioner executes — never reverse** (`adr/0020` pt2).
  Nothing to claim: queue is gone.
- **Hub-and-spoke for lifecycle: nothing commands provisioner except `core`.**
  Pod create/teardown routes agent → sidecar → core → provisioner (`adr/0020` pt4).
- **MCP is local, full stop** (`adr/0048` §6). Sandbox merged into worker pod → no
  MCP crosses a pod boundary; provisioner speaks no MCP. gRPC remains the only
  inter-component protocol.
- **No prospective Write/Edit gate — `canUseTool` reproduces the SDK's own live prompt**
  (`adr/0029`, superseding `adr/0021`). Sessions start `"default"` (CLI parity); the SDK's
  `permissionMode` decides when `canUseTool` fires; it asks a human and blocks. Never
  inferred from silence or round completion — a decision is always a real structured
  `RespondToPermission` or `SetPermissionMode`. `/approve` gone.
- **Gate is `canUseTool`, not a rule list the SDK re-interprets** (`adr/0053`). Two
  answers are the fleet's own and never reach a human: its **own** MCP tools
  (`mcp__agent-fleet-sidecar__*`, `mcp__playwright__*`) always allowed — else the agent
  needs permission before it can *ask* for permission, which is what shipped — and in
  `auto`, everything except `Bash` running `rm`/`sudo`. `allowedTools` still lists the MCP
  tools but nothing depends on it. `bypassPermissions` deleted: bought only `rm`/`sudo`
  over `auto`, charged a launch profile for them.
- **No sandbox** (`adr/0048` §6, superseding `adr/0039`/`0044`/`0045`). Builds, tests and
  installs run in the worker pod's own `Bash`, gated by `canUseTool`. `run_command`,
  `request_e2e_env`, `kill_env` deleted. Three ADRs went into repairing a second pod whose
  whole purpose was letting **one tool** skip a prompt; the fix was deleting the pod.
- **Session with no message has no pod — that is the human gate** (`adr/0048` §1).
  `CreateSession` writes a row, nothing else; first message provisions. Ordering is
  **warm-then-append, never reverse**: `resumeFromSeq` = `LatestSeq` at dispatch, so a
  message appended before the pod exists lands below its cursor and is never
  delivered. `PostMessage`/`OpenFromProposal`/`PromptSession` all carry the comment —
  copy it, don't re-derive.
- **Dashboard is a console; the list answers decisions** (`adr/0042`). Blocked session
  renders its pending permission/question inline, answerable without opening it —
  blocked = stalled until a human clicks. Five feed tiers stay visually distinct; a new
  entry kind belongs in a tier, not a uniform grey log line. Anything both form factors
  show goes in shared components.
- **Console gate lives in `core`, not at ingress** (`adr/0056`). `fleet.bnei.dev` has
  exactly one lock now — `basic-admin-auth` gone — and the IngressRoute matches on host
  with no path constraint, so **a new exempt path is a new public endpoint**. Unset OIDC
  config → core refuses to start. `FLEET_AUTH_DISABLED=1` = explicit local-stack opt-out.
- **Nothing on `CoreService` is callable without the session's `lease_id`** (`adr/0057`).
  Authorization is an explicit per-method table — a field-name rule is wrong on
  `PromptSession`, `SaveAgentSessionId` and `GetSession`, each time in the direction that
  grants authority. A method in no table fails closed.
- **`X-authentik-*` headers never read.** Pod-network caller can forge one → authz gap
  becomes impersonation. core verifies its own ID token instead.
- Every result = PR.
- One PVC per session — never a shared writable checkout across concurrent sessions.
  Fleet creates no branches either: agent runs its own `git checkout -b` (`adr/0048` §5).
- Git commit identity derived live from authenticated bot GitHub account (`gh api user`),
  never hardcoded. Done independently by provisioner (clone/fetch) and worker (push/PR) —
  separate pods, no shared `$HOME`.
- Claude Code auths via subscription OAuth (`CLAUDE_CODE_OAUTH_TOKEN`), not a metered API
  key. No cost cap by design.
- Secrets only via Infisical (project `agent-fleet-nygh`). Never committed.

## Forbidden patterns

Quick check. Full list + reasons in `agent-fleet/docs/DECISIONS.md`.

Written plainly on purpose — every line here is a thing that has gone wrong, or would
go wrong, and a fragment that gets misread is worse than a few extra words.

- A shared writable repo volume across sessions.
- Inferring a permission decision from silence.
- Appending a message before the pod that must read it exists.
- Sending a human's instruction as `description` — the agent never reads that column.
- Hardcoded git commit identity.
- Committing Discord, GitHub or Anthropic tokens.
- A bespoke per-app Helm chart — reuse `infra-bootstrap/gitops/platform/common-app-chart`.
- Any component other than `core` holding `AGENTFLEET_DB_*` credentials.
- A worker or sidecar *commanding* the provisioner directly — pod lifecycle must route
  through `core`.
- A second pod so that one tool can skip a permission prompt (`docs/adr/0048` §6).
- Reintroducing a status enum, a queue, a lease-renewal heartbeat, or a retry counter
  (`docs/adr/0048` §2).
- Fleet-managed `infra-bootstrap` cluster ops — kubespray, ansible, pigsty. That is the
  parent repo's own `CLAUDE.md` boundary, and it is blocked here too.

## Current targets

`dream-analyst`, `vos-monolith`, `agent-fleet`. Real repos, seeded into the
dashboard-editable `repos` table (`adr/0028`) — no redeploy to add/edit one. No per-repo
Deployment/PVC: onboarding = a "manage repos" entry, not new k8s manifests.

## Verification traps

Green CI necessary, not sufficient. Every one below was **silent** — check passed, or
wasn't run, or measured wrong thing. Full account, with what guards each:
**`agent-fleet/docs/verification-traps.md`**. Read that before trusting a green run on
deploy, CI, permission or pod-lifecycle paths.

- `tsc --noEmit` ≠ build. Real cmd `bun run build` (`tsc -b && vite build`).
- PR build never pushes an image. Check registry, not CI. Only last 3 tags + `latest` kept.
- `kubectl apply --dry-run=server` creates no pod. ArgoCD `Synced`+`Degraded` = the tell.
- Unset GitHub secret = empty string, not error. Non-secret CI needs → `env:`, not `secrets:`.
- New component → wire into **every** CI path (`docker.yml` `COMPONENTS` + `changes` filter
  + `go.yml` codegen). Guard `core/internal/buildguard`, run `-count=1`.
- **Squash-merging a stacked PR auto-closes the PR above it, and GitHub will not reopen
  it.** Retarget dependent PRs to `main` *before* merging the one below.
- **A squash also drops any change a stacked branch made and then undid**, with no
  conflict. After merging a stack, `ls` the files the upper PR claimed to delete — the PR
  diff will not tell you.
- Bound port ≠ reachable service ≠ working capability. Verify the capability doing its job.
- Wide rename can swap two same-typed fields silently. Check call sites where old and new
  names now collide.
- `process.exitCode = 1` ≠ exit. Single-shot process → exit explicitly.
- Fleet wedges silently only after `MAX_LIVE_SESSIONS`. Test = run six, open a seventh.
- Nullable column → nullable scan or `COALESCE`. One NULL fails the whole query, not the row.
- Feature can be fully dead with every test green if nothing drives the UI. Run
  `/dashboard-e2e` after touching create, decision or warm paths.
- Config value can exist at every layer and reach nothing. Trace it to its consumer.
- Deleting a pod deletes its resource envelope; moved work keeps old limits. cgroup v2 OOM
  kills the whole container — no error line, logs just stop. Confirm via node `dmesg`.
- Terminal Job deleted on sight takes the evidence with it.
- `fsGroup` doesn't change a file's owner. Fake clientset never runs the script it asserts on.
- `MERGEABLE` ≠ compiles. Semantic collision gives no conflict signal. Build the merge
  result, not the branch.
- OIDC issuer compared byte-for-byte → normalising it is a change. Fails closed = outage.
- `fleet.bnei.dev` has one lock, IngressRoute matches host with no path constraint. **A new
  exempt path is a new public endpoint.**

## Workflow rules

- All changes via feature branch + PR. No direct push to `main`.
- Secrets never committed; fetched from Infisical at run time.
- Conventional Commits — `release.yml` runs `release-it` off them, bumping
  version/CHANGELOG on every push to `main`.
- AI co-author trailer: `Co-Authored-By: ukubi-claude-macbook <noreply@bnei.dev>` for work
  on this repo. `ukubi-agent` is only for fleet worker pods.
- `worker/`: `bun:test` for coordination logic cheap to mock (SDK streaming-input `query()`,
  sidecar local HTTP client). Runs in CI ahead of Docker build. Also `bun run typecheck`.
- `core/`, `provisioner/`, `sidecar/`: `go test`. Table-driven units, `client-go` fake
  clientset for manifest shape, `bufconn` for gRPC roundtrips, `testcontainers-go`
  integration tests (`-tags=integration`) against real Postgres. `golangci-lint` alongside.
- **Postgres schema's sole source of truth is `db/migrations/`.** A schema change is always
  a new numbered `.up.sql`/`.down.sql` pair applied via golang-migrate — never an edit to an
  existing migration, never a hand-rolled `CREATE TABLE` test fixture, never any other copy.
  Integration tests use `core/internal/dbtest.NewPool(t)`. CI-enforced by construction (one
  real source, not a diff-check) after two incidents where a hand-copied schema shipped
  missing a column (`adr/0030`).

## Skills

- `/fleet-ops` — onboard a target repo, walk release/deploy, inspect what's live.
- `/fleet-feature` — checklist for adding to `core/`/`provisioner/`/`sidecar/`/`worker/`
  (new gRPC method, MCP tool, slash command, transcript entry type).
- `/fleet-debug` — diagnose stuck/failed session (session+journal state, worker/sidecar/
  provisioner logs, `transcript` table, known failure modes).
- `/dashboard-e2e` — minimal local stack (throwaway Postgres + stub provisioner + `core` +
  dashboard dev server) to Playwright-test the UI, then tear down. **Run after touching
  create, decision or warm paths** — see traps.
- `/kind-local` — disposable kind cluster running the real fleet, for the pod-dispatch path
  `/dashboard-e2e` fakes.
