# Reliability findings

Backlog of correctness/reliability gaps found auditing the fleet's
happy path (task creation → PR), started 2026-08-05. Not an ADR log —
verified findings, not settled decisions. Promote to `adr/` once a fix
lands; until then, backlog.

Status: `open` (found, undesigned) · `designing` (decision drafted) ·
`fixed` (landed, PR linked).

> **Read this first (2026-08-15).** This backlog predates
> [`adr/0048`](adr/0048-one-session-one-pod-one-shared-home.md), which
> resolved several findings by **deleting the mechanism they were about**
> rather than by fixing them. Findings 1, 2, 5, 6 and 12 all concern the
> dispatch queue, the lease/heartbeat/reclaim machine, worktree lifecycle or
> `retry_count` — none of which exist. `CreateTask`, `ClaimNextTask` and
> `CountInFlight` are gone with them, so any symbol below that you cannot
> grep for was deleted, not renamed. The findings are kept as written
> because the *reasoning* is what made the case for deleting them; treat the
> proposed fixes as history, not as a to-do list.

---

## 0. Worker session = plain Claude Code session. Discord/dashboard = transport only.

**Status:** fixed → see [`adr/0025`](adr/0025-continuous-session-worker-redesign.md). See PR #29. All five "must fix before implementation"
open items resolved as part of that PR (including a real SDK/transport
spike — held a canUseTool decision pending for 3 minutes, no timeout or
dropped connection). Still worth a real ADR later (revises ADR-0005's
mechanism, ADR-0021's phase design) — this backlog entry stays as the
implementation record until then.

**Principle:** Claude Code already handles permission prompts,
questions, stop — headless included. Fleet shouldn't build protocol on
top (phase state machine, magic strings, text-intent inference) — just
relay the real session through Discord/dashboard, uniformly. No "THE
approval gate" vs "just a question."

**Deletes** (supersedes #3, #4 — same symptoms, mechanism removed not
patched):
- `permissionMode: "plan"→"default"` as fleet-orchestrated state,
  `plannerPrompt`/`implementPrompt` split (`planning.ts:83-112`). One
  continuous session, no fleet-imposed phase.
- `PLAN_READY:`/`PR_READY:` parsing + round-cap/checkpoint machinery
  (`awaitingCheckpointReply`, `MAX_PLANNING_ROUNDS`). No phase boundary
  to signal.
- `isApproval`/`isAbort` free-text regex (`planning.ts:33-43`). `/approve`,
  `/stop` stay as structured signals (`type:"approve"`/`"abort"`,
  already clean); free text is just conversation, never auto-triggers.

**Keeps as the real gate — delegate to Claude Code's own permissions,
don't reinvent one:** `Write`/`Edit` stay excluded from `allowedTools`
in the initial posture — already today's real SDK-level gate
(`planning.ts:174-179`'s own comment: `allowedTools` bypasses
`canUseTool` entirely, so listing them there removes the gate, not adds
one). Not fleet business logic, existing SDK mechanism. What changes:
the *trigger* to unlock it — a clean agent-initiated `AskUserQuestion`
confirmation, not regex-matched text. Backed by two more reasons this
is sufficient: blast radius is small (writes confined to one pod's own
worktree, cheap/reversible until an explicit push), and the one
consequential action (pushing a protected branch) already has its own
infra-level net — git/GitHub branch protection — independent of
fleet code.

**Replaces approval/questions with:** the already-working, already
channel-agnostic `AskUserQuestion` mechanism. `AnswerQuestion`
(`core/internal/dashboard/server.go:172-178`) is a 3-line wrapper
around the same `transcr.Append` Discord's own `relay()` already calls
— nothing dashboard-specific. Fix: Discord tags a reply `type:"answer"`
(not `"discussion"`) when a question is pending for that task, **with
question-seq correlation** — today's naive "any pending question + any
reply" would let an unrelated message satisfy it (verified against both
`AskUserQuestion`'s long-poll and `AnswerQuestion`'s single-pending
assumption — real gap, not hypothetical, must fix before shipping).

**Component model:**
- **Agent**: sandbox = worktree. Tools = native + sidecar-proxied MCP.
  Full autonomy over sequence/timing — explore, plan, ask, implement,
  test, commit, push, PR, done. No fleet phase, no fleet gate.
- **Worker TS** (`worker/src/`): bootstraps `query()`, keeps heartbeat
  alive (dumb unconditional timer — can't move to the agent, must prove
  *process* liveness independent of agent turn state), exits when
  session ends. Talks only to the sidecar (already ADR-0020). `pushAndOpenPr`/
  `configureGitAuth`/`run()` (`index.ts:29-61`) deleted — agent runs
  `git commit`/`push`/`gh pr create` itself via Bash, whenever it
  decides, same as this session. `configureGitAuth` moves to run once,
  unconditionally, before the session starts (not lazily pre-push).
- **Sidecar**: link to the exterior (all outbound gRPC) + pod-infra
  surface, unchanged shape. Not a new *kind* of responsibility —
  `send_message` already proves the pattern with zero wrapper code
  (SDK's own MCP client → sidecar directly), `streamHumanMessages`
  proves the reverse (SSE → `input.push()`). New tools ride the same
  rails.

**Concerns segregation:** agent decides *when*, infra guarantees
*correctness*. Never bury an infra-motivated guard in the agent-facing
tool surface. Push/PR = plain Bash, like `commit` — no lease-check tool
(rejected: agent-dependent safety isn't safety; bundling the check into
one atomic "ship it" tool still guards inside the pod — wrong layer
either way). Stale/reclaimed-pod duplicate-work risk is handled at the
infra layer (#1's faster crash detection shrinks the window), not the
pod — residual case is a human closing a duplicate PR, not a lock.
"Ship it" isn't terminal — PRs get reviewed and iterated on, possibly
via a *new* task reusing the same branch (#2's reuse-not-wipe already
supports this). "Human decides it's over" = #2's git-sync `[gone]`
detection — not a new mechanism.

**Raw SDK output stream — relay everything, let the UI decide, no
pre-filtering.** `logSdkMessage` (`planning.ts:114-149`) already
observes every message live; today only relays `assistant`+`text`
(line 132), rest is locally logged only. Fix: relay every message,
tagged by type, same `pushMessage` path `PushToolTelemetry` already
uses — no new RPC. Wrapper's `for await` loop can't disappear (`Query
extends AsyncGenerator<SDKMessage, void>`, something must consume it)
but becomes a dumb unconditional forward, no per-message judgment.
**Discord's relay filter must flip denylist → allowlist** — today it
only skips `tool_call` by name; raw `tool_result` (Bash stdout/stderr,
file contents) would post verbatim to Discord for any new type nobody
remembered to exclude. Real secret-leak risk. Transcript itself stays
unfiltered regardless — dashboard renders full stream.

**Open, must fix before implementation** (doubt-driven review,
2026-08-05, single-model, cross-model skipped by choice):
- No status-reporting path in the reduced `main()` — a successful task
  can sit `implementing` forever, burning a concurrency slot until a
  duplicate pod gets dispatched on top of an already-done one. Needs an
  explicit status-set on session end.
- Discord answer-tagging needs question-seq correlation (above).
- Discord relay needs to be an allowlist (above).
- `BASE_BRANCH` (per-repo, e.g. `vos-monolith` uses `dev`) and a
  real-PR-resulted check both silently vanish once the wrapper stops
  orchestrating push/PR — need threading into agent context + some
  verification step.
- Unverified: does blocking a permission decision for minutes inside
  one continuous `query()` session hit any SDK/transport timeout?
  Doesn't change direction if fine, needs checking before build.

## 1. Worker-pod failure handling: fragmented across three uncoordinated mechanisms

**Status:** fixed → see [`adr/0024`](adr/0024-crash-fast-path-and-journal-read.md). See PR #28.

**Where:** `provisioner/internal/reconcile/loop.go` (10s poll, GCs dead
pods, tells no one, skips worktree cleanup) · `core/internal/coreserver/
server.go:193-218` (`SetTaskStatus`'s teardown — only fires if the
worker itself writes a terminal status) · `core/internal/tasks/
store.go:135-162` (`ClaimNextTask`'s 10-min heartbeat reclaim — the
*only* real retry path).

**Problem:** nothing watches a scheduled pod's live status. A mid-task
crash is invisible to `core` for up to 10 minutes. `knowledge_journal`
gets pod-lifecycle events (`ReportEvent`/`ReportPodEvents`) but has no
read RPC anywhere — even a journaled crash is invisible without direct
Postgres access. `retry_count` tracked, never capped.

**Decision:** reconcile loop reports terminal pod phases to `core`
immediately as a fast-path accelerant *on top of*, not replacing, the
heartbeat fallback (preserves ADR-0020 pt.3 — Postgres stays sole,
restart-durable ground truth). Retry cap → new terminal status (e.g.
`failed_permanently`). Discord scope not expanded — crash visibility
surfaces in the dashboard only (explicit user call).

**Read-path fix:** not a missing mechanism — `GetTranscript`/
`StreamTranscript` (`dashboard.proto:124,129`) already prove a typed
query surface works, and sidecar telemetry (`PushToolTelemetry`)
already flows through it. Gap is narrower: `knowledge_journal` has no
`Get`/`List` RPC, and provisioner-owned facts (worktree list, live pod
phase) have zero query path — `core` has no PVC/cluster access.
Rejected: a generic `Query(bytes) returns (bytes)` dispatcher — not
justified by two concrete gaps, throws away protobuf's type safety.
Fix: two more typed RPCs, same pattern as `GetTranscript`/
`GetE2eSessionStatus` — a `knowledge_journal` read RPC (here),
`ListWorktrees`/worker-status RPCs (shared with #2).

## 2. Worktree/branch lifecycle: status-triggered deletion loses data — redesigned around explicit signals only

**Status:** fixed → see [`adr/0023`](adr/0023-worktree-reuse-and-branch-sweep.md). See PR #27.

**Old bug:** `RemoveWorktree` (`git.go:164-177`) unconditionally
`branch -D`s on every terminal status. Commits survive worktree
deletion (they live in the shared object DB, `CreateWorktree`'s
`branchExists` reattachment already relies on this correctly) — but
`branch -D` on a `status="failed"` reached via a `git push` failure
destroys the *only* reference to never-pushed commits, permanently.
Separately, `CreateWorktree` (`git.go:139`) unconditionally
`RemoveAll`s on every retry, destroying uncommitted work even on a
same-task-ID crash-retry where the branch itself is correctly reused.

**New design:**
- **`CreateWorktree`: reuse, don't wipe, don't validate.** Path exists
  → return as-is, zero git commands, zero validity check. Stale
  `.git/index.lock`, half-written files — Claude Code's problem inside
  its own pod (Bash access), not the provisioner's. `plannerPrompt`
  (`planning.ts:87`) needs to drop its unconditional "you are in a
  fresh git worktree" claim so a resumed agent actually checks `git
  status`/`git log` first.
- **Teardown stops touching git state entirely.** `tearDownWorker`
  (`grpcserver/server.go:198-214`) only deletes the pod, for any
  status including `done`. Two things replace `RemoveWorktree`:
  1. **Automated** — new periodic sweep (own package, `git.Manager`,
     same per-repo mutex, few-minute interval): `git fetch --prune
     origin`, then `git for-each-ref --format='%(refname:short)
     %(upstream:track)' refs/heads/agent/`. `[gone]` → delete branch +
     worktree together. Order matters: `git worktree remove` (updates
     git metadata) *before* `branch -D` — git refuses `-D` on a
     checked-out branch, wrong order means the sweep silently deletes
     nothing. Don't hold the mutex for the `fetch` itself (network
     I/O) — only for the mutations after — or a slow fetch stalls live
     task dispatch on that repo.
  2. **Manual** — dashboard "worktrees" view. `core` has no PVC access,
     so: dashboard → `DashboardService` → new `ListWorktrees` on
     `ProvisionerService` (task/repo/branch/upstream-track/mtime),
     **left-joined** in `core` against `tasks` (status/error/pr_url) —
     inner join would hide exactly the orphaned worktrees this tool
     exists to surface. `DeleteWorktree(taskID, alsoDeleteBranch bool)`
     — per-call checkbox, doesn't touch the `tasks` row. Both new RPC
     handlers must take the same per-repo mutex as every other git op.
- **Accepted gap, not fixed:** `git worktree add -b <branch> <path>
  origin/<base>` tracks `origin/<base>` by git default, not a branch
  that doesn't exist yet — so `[gone]` can never fire for work
  abandoned *before* its first push. Every pre-push crash/fail/cancel
  leaks its worktree forever, invisible to the sweep. Decision: leave
  it. Manual dashboard delete is the primary cleanup path; an optional
  dumb mtime-based cron sweep (no DB lookup, keeps zero-Postgres-access
  intact) is the fallback if manual isn't enough in practice. Not
  solved by making git-sync smarter.

## 3. `isApproval`/`isAbort`: whole-word substring matching, not intent parsing

**Status:** fixed — superseded by #0, mechanism deleted (not hardened) in PR #29.

`planning.ts:33-43`: `/\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b/i`
on free text. "I don't approve this, redo the auth flow" contains
"approve" → false-positive unlock. Same bug re-fires mid-implementation
too (no `!approved` guard on the check) — any later message matching
the regex re-pushes the full implement prompt as if fresh. #0 removes
the free-text branch entirely; `/approve`/`/stop` stay as explicit,
structured signals.

## 4. `PLAN_READY:`/`PR_READY:` magic string prefixes

**Status:** fixed — superseded by #0, no phase boundary left to signal, in PR #29.

`planning.ts:25,92,254-255`, `index.ts:82`:
`result.summary.split("PR_READY:")[1]?.trim() ?? result.summary`. No
error path on mismatch — wrong casing/paraphrase silently dumps the
entire raw transcript into the PR body instead of erroring. Moot once
#0 removes the phase split "task done" is just the session's natural
final message.

## 5. Two independent, uncoordinated 2s pollers instead of event-driven nudges

**Status:** fixed. See PR #25.

`core/internal/dispatch/loop.go:34` (task claiming), `core/internal/
transcript/relay.go:24-35` (Discord relay) — both hardcoded 2s tickers
(`run.go:50,57`). Up to 2s latency each, same "poll a table" pattern
duplicated, uncoordinated. **Fix:** keep both tickers (dispatch's still
needed for heartbeat-reclaim scanning) but nudge directly after the
write — `CreateTask` calls dispatch's tick, `transcript.Append` calls
relay's flush. Two small additions.

## 6. `CountInFlight`/`ClaimNextTask` not in one transaction

**Status:** fixed. See PR #25.

`dispatch/loop.go:47,56`. Not exploitable single-replica (`ClaimNextTask`'s
`SKIP LOCKED` prevents double-claiming the *same* task) — but >1 replica
could transiently exceed `MAX_IN_FLIGHT_TASKS`. **Fix:** one transaction,
or fold the headroom check into `ClaimNextTask`'s own `WHERE`. Low
priority unless `core` scales beyond one replica.

## 7. Sidecar telemetry/journal calls fire-and-forget; journal has no read path

**Status:** fixed. Swallowed errors now logged in PR #25; read path landed with #1 in PR #28.

`index.ts:65,78,97,107,111`, `planning.ts:132,245,273-277` —
`appendJournal`/`pushMessage`/`saveSessionId` all `.catch(() => {})`'d.
A failed write is invisible twice: swallowed locally, and (per #1)
`knowledge_journal` has no read path even if it succeeded. **Fix:**
covered by #1's read-path work; at minimum log (don't swallow) locally.

## 8. Non-`success` SDK result subtypes unhandled outside the approved branch

**Status:** fixed — resolved as a byproduct of #0 removing the branch split, in PR #29.

`planning.ts:260-282`. Real subtypes confirmed against SDK types
(`coreTypes.d.ts:458-460`): `error_during_execution`, `error_max_turns`,
`error_max_budget_usd`, `error_max_structured_output_retries` — can
occur anywhere, not just after `MAX_TURNS` (unset today). Only the
(now-superseded) `if (approved)` branch checks `msg.subtype`; nothing
else does. Unclear if the SDK's iterable ends or hangs on such a
result — either way unhandled. **Fix:** treat any non-`success` subtype
as an error uniformly, everywhere, once #0 removes the branch split.

## 9. Sidecar unreachability at the worst moment cascades into total silence

**Status:** fixed. See PR #25.

`index.ts:88,110` — `stillHoldsLease` and the catch block's
`setStatus("failed", ...)` are bare, unguarded (unlike everything
else's `.catch(() => {})`). A sidecar blip right as a task finishes:
`stillHoldsLease` throws → outer catch tries `setStatus("failed")` →
same outage, throws too → escapes to `main().catch()` uncaught → task
status never updates, stuck `implementing` until the 10-min reclaim.
**Fix:** last-resort retry or a final best-effort `setStatus("failed")`
in the top-level crash handler.

## 10. `gh pr create`'s PR-URL extraction can grab the wrong URL

**Status:** fixed. See PR #25.

`index.ts:58` — `stdout.match(/https:\/\/github\.com\/\S+/)` grabs the
*first* GitHub URL in stdout, not necessarily the PR's. **Fix:** `gh pr
create --json url -q .url` (structured output) instead of regexing
free text.

## 11. Provisioner hand-rolls ephemeral pod lifecycle instead of using `batch/v1.Job` — CRD/operator considered and rejected for now

**Status:** fixed → see [`adr/0022`](adr/0022-batch-job-worker-pod-lifecycle.md). See PR #26.

**Where:** `provisioner/internal/reconcile/loop.go` — same code as #1, different angle: it creates bare `Pod`s directly and hand-rolls retry/GC/status via a 10s poll, duplicated across the two pod kinds (worker, e2e-preview).

**Question raised:** whether a full Kubernetes operator (custom CRD + `controller-runtime` reconcile loop) should own pod lifecycle instead — either replacing this part of the provisioner or running alongside it, motivated by concern separation (git/PVC logic tangled with pod-templating logic), cognitive burden of two near-duplicate pod-kind code paths, and a speculative future "on-demand infra-DevOps agent" feature (agent reacts to cluster alerts, might want deep K8s API visibility).

**Decision:** adopt `batch/v1.Job` (`ttlSecondsAfterFinished` for GC, `backoffLimit`/`status.conditions` for retry and terminal-state detection) in place of hand-rolled bare-`Pod` create/poll/GC, and swap the reconcile loop's 10s poll for an informer/watch on `Job` status. **No custom CRD, no `controller-runtime`, no new deployable.** Crash-detection latency explicitly not the driver here — worker already reports to `core` directly; #1 already covers the reconcile-loop→`core` reporting gap.

**Alternatives considered:**
- *Informers on bare `Pod`s, no CRD* — event-driven instead of polling, but still hand-rolls the retry/backoff/GC semantics `Job` already gives for free. Superseded by the chosen option, not wrong, just strictly worse than using `Job`.
- *CRD as a passive mailbox + thin controller* (spec→pod mechanics only, no autonomy) — rejected: no consumer needs `kubectl`-native visibility today. The dashboard gets pod/worktree state through `core`'s RPC surface (`ListWorktrees`, shared with #2's `ProvisionerService` work), and `core` deliberately holds no cluster RBAC to read CRs directly anyway (ADR-0020 pt.1) — a CR's `.status` would still need relaying through the same RPC hop, not bypass it.
- *Full autonomous CRD+operator* — rejected: operational cost (CRD schema is a one-way door once anything depends on its shape; leader election; new cluster-scoped RBAC surface) not justified at current scale (concurrency cap 5, single provisioner replica); risks blurring the locked "provisioner never decides pod lifecycle on its own" invariant (ADR-0020 pt.2), since an operator's whole point is autonomous convergence. The speculated future driver (alert-triggered DevOps agent) turned out on inspection to be a separate concern — what RBAC/tooling *that agent's own worker pod* gets for live cluster diagnostics — orthogonal to how agent-fleet's own ephemeral pods are created, and out of scope here.

**Consequences:** provisioner keeps owning pod lifecycle (no new component to build/deploy/monitor) but gets `Job`'s retry/backoff/GC semantics for free instead of hand-rolling them; e2e-preview and worker pods both templated as `Job`s, same shared code path. Doesn't eliminate the reconcile-loop→`core` reporting hop (#1 covers that separately) — just removes the hand-rolled mechanics underneath it.

**Reversibility:** two-way door — `Job` is a built-in resource, nothing to migrate away from. A CRD/operator remains addable later (e.g. if a future incident-remediation feature genuinely needs externally-authored declarative desired state or `kubectl`-native visibility) without unwinding this.

## 12. Session resume is dead code — a reclaimed task always starts a brand-new Claude session, and retry_count doesn't distinguish "crashed" from "idle"

**Status:** open.

**Where:** `docs/adr/0016-task-crash-recovery-and-retry.md` (the original design) · `docs/adr/0021` lines ~162-166 (flags this as an unverified open risk after the continuous-session rewrite) · `core/internal/tasks/store.go`'s `SaveSessionID`/`planning_session_id` column and `ClaimNextTask`'s `retry_count` increment · `provisioner/internal/k8s/pod.go`'s `CreateWorkerPod` env-var list · `worker/src/types.ts`'s `Task` type · `worker/src/planning.ts`'s `query()` call · `sidecar/internal/mcpserver/server.go`'s `AskUserQuestion`/`wait_for_messages` long-poll.

**Problem:** ADR-0016 designed a resume path — Claude session transcripts persisted on the worker PVC via `CLAUDE_CONFIG_DIR`, with `planning_session_id` as a Postgres-durable pointer so a reclaimed task could `resume:` its prior session. None of it survived the later rewrites: `CLAUDE_CONFIG_DIR` is set nowhere in the repo, `SaveSessionID` writes `planning_session_id` but nothing ever reads it back, `CreateWorkerPod` passes no session id to the new pod, `worker/src/types.ts`'s own `Task` comment says a single-shot worker "no longer claims, retries, or tracks its own resume state," and `planning.ts`'s `query()` call has no `resume:` option. **A task reclaimed today — for any reason, including a pod simply being killed — starts a completely fresh Claude session**, re-deriving context only from git state on the (reused) worktree. All prior reasoning/plan discussion is gone, not paused.

Compounding this: `ClaimNextTask` increments `retry_count` on every reclaim of a stale-heartbeat task, capped by `MAX_TASK_RETRIES` (default 3) before the task flips to `failed_permanently` — permanently, unrecoverably. Nothing distinguishes "worker pod genuinely crashed" from "worker pod was killed/died while the task was legitimately just waiting on a human" (an open `AskUserQuestion` — `wait_for_messages` times out and returns `{"status":"pending"}` with no forced exit — or an unactioned plan-mode approval gate). A task can also sit with a *live* pod indefinitely today: nothing exits the worker process on its own after a long idle stretch (no `ActiveDeadlineSeconds` on the worker `Job`, no self-initiated idle-exit), so the only way this gap gets exercised right now is an involuntary pod death (node pressure, OOM, `kubectl delete`, a future stuck-pod GC pass) while a task is mid-thought.

**Why it matters now:** came up designing a stuck-pod GC for `provisioner/internal/reconcile`'s reconcile loop (`fix/worker-pod-stop-gc` branch) — killing a worker Job that's been `Running`/`Pending` past some age ceiling, to catch a task nobody will ever click Stop on. Rejected for now specifically because of this gap: with no real resume, the GC would silently discard in-progress reasoning on every kill, and repeated kills of a task the human is legitimately still thinking about would erode `retry_count` toward `failed_permanently` for reasons unrelated to any real failure. That PR ships only the Stop-initiated grace-period force-kill (`dispatch.Loop`'s `enforceStopGrace` — safe, since it only fires after an explicit human Stop, never an idle-but-intentional pod) and leaves stuck-pod GC as follow-up work blocked on this.

**Open question:** what's the right fix, and in what order —
1. Actually wire up ADR-0016's resume path for the current continuous-session architecture (`CLAUDE_CONFIG_DIR` on the shared PVC, read `planning_session_id` back on `CreateWorkerPod`, pass `resume:` to `query()`) — closes the context-loss half, but ADR-0021 already flagged this path as unverified even in its original crash-only scope; may need real design work for the continuous-session case.
2. Separately, stop conflating "crashed" and "idle-on-human" in `retry_count`/reclaim accounting — e.g. a distinct "blocked on human" status or signal (open question/pending approval in `planning_transcript`) that reclaim and any future stuck-pod GC both treat as "don't count against retries, don't age out" or "age out on a much longer clock."
3. Only once both land does a stuck-pod max-age GC (the original motivation) become safe to build without either losing work or silently killing tasks the human just hasn't gotten to yet.

---

## Already fixed

- **Worker/sidecar pod startup race** — `sidecar` now a native K8s
  sidecar (`InitContainers` + `RestartPolicy: Always` + `StartupProbe`).
  `agent-fleet` PR #24.
- **`core`'s Service missing its gRPC port (9090)** — broke
  `ReportEvent`/`StillHoldsLease` on every task. Fixed via
  `common-app-chart`'s `service.extraPorts`. `infra-bootstrap` PR #108,
  `agent-fleet` PR #24.
