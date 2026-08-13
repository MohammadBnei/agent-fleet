# ADR-0044: The e2e pod is a sandbox that may also run an app

**Status:** Accepted
**Date:** 2026-08-13
**Builds on:** [ADR-0034](0034-environment-recipe-system.md) (profiles are the
source of truth), [ADR-0036](0036-e2e-recipe-visible-and-override-approved.md)
(the readiness probe and the override gate), [ADR-0039](0039-e2e-pod-is-the-worker-sandbox.md)
(the pod became the worker's build/test sandbox)

## Context

The e2e pod was unreliable in a way that read as flakiness and wasn't: "it
just won't start on certain conditions, like the profile" was a set of
reproducible dead ends, all downstream of one mistake.

**The pod's lifetime was coupled to the target app's process.** ADR-0039 made
this pod the worker's build/test sandbox — `run_command` is registered
statically for every session, from turn one, resumed sessions included. That
made the sandbox the pod's *primary* job and the app preview its *secondary*
one. But every part of the pod's construction still treated the app as the
reason the pod existed, so the secondary role could kill the primary one:

1. **`entrypoint.sh` ended in `wait -n` under `set -e`.** `wait -n` returns as
   soon as the *first* background job exits, so a failing `bun install`, a
   one-shot start command, or a crashed dev server took PID 1 with it —
   `RestartPolicy: Never`, pod `Failed`. ADR-0036 chose readiness-over-liveness
   specifically so "a pod whose app never binds must stay alive so code-server
   is still reachable to debug why". That guarantee only ever held for an app
   that *hangs*, never one that *exits*.

2. **A dead pod was a permanent dead end.** `CreateE2ESession`'s idempotent
   early-return treated any non-`Terminating` pod as a live session, including
   `Failed`/`Succeeded`. It handed back the corpse's preview URL on every
   subsequent call; `run_command` retried into an unreachable pod for the rest
   of the session; nothing GC'd it. Only `kill_env` broke the loop.

3. **A repo with no `"e2e"` profile could never start a sandbox at all.** Core
   hardcoded the profile name; `repoprofiles.Store.Get` returns `nil, nil` on
   a miss; an empty `start_cmd` was a hard error in `CreatePod`. agent-fleet
   (whose toolchain lives on its `"lint"` profile) and infra-bootstrap were
   exactly this. ADR-0039 listed this as a known gap; it is not a gap, it is
   the sandbox being unavailable on the fleet's own repo.

4. **The first `run_command` of a session almost always errored.** The handler
   provisioned, then retried once with **zero delay**. `RequestE2eEnv` returns
   the moment the pod *object* exists — Pending, with scheduling, image pull
   and init containers still ahead of it — so the retry could not land.

5. **Status lied.** `CreateE2ESession` returned a hardcoded `"running"` for a
   Pending pod, and `e2eStatusFromPhase` mapped `PodUnknown` to `"running"`
   too.

6. **`CreateE2eSession` had no client deadline** — the same defect
   `sessionCallTimeout` was introduced to fix, on the path nobody had audited.

7. **e2e pods were never garbage-collected.** At 1000m/1Gi requests each,
   leaked sandboxes are what makes the *next* pod sit `Pending` forever, which
   presents to a human as "the e2e pod won't start."

Separately, and load-bearing for the fix: `run_command 'bun run dev &'` never
worked. `execmcp` sets `cmd.Stdout` to a `bytes.Buffer`, so `os/exec` wires
real OS pipes and `Wait` blocks until every writer closes them — a
backgrounded grandchild inherits those pipes and holds them for its whole
life. The trailing-`&` convention the tool's own description instructs the
agent to use blocked for the full 15-minute timeout and then still didn't
return.

## Decision

**The e2e pod is a sandbox that may also run an app.** Six parts:

### 1. PID 1 outlives every child; nothing inside the container ends the pod

`entrypoint.sh` drops `set -e` (with a supervise loop, `-e` would exit the
background subshell on the first failure and silently *stop* supervising —
the same bug in a new hat) and ends on a bare `wait`. The three servers —
code-server, Playwright MCP, execmcp — get a `supervise()` restart loop: they
are idempotent, instant to start and side-effect-free, so restarting them is
always right. Pod lifetime is owned exclusively by whoever deletes the pod:
`kill_env`, core's teardowns, or the new sweep.

**Rejected: anchoring PID 1 to execmcp** (`wait $exec_pid`). It is the least
wrong single anchor and still wrong — execmcp dying would take down the
782s-warm dev server and the code-server debug surface ADR-0036 exists to
preserve. "Which one process should be allowed to kill the pod" is the
question with no good answer; the answer is none.

### 2. The app runs once and is not restarted

Output goes to `/tmp/e2e-app.log` **and** the container's stdout (via `tee`),
with an explicit exit-status marker line.

**Rejected: restart-with-backoff.** The failures this pod actually has are
deterministic — a failing install, a command binding `127.0.0.1`, a stale
profile. Retrying re-runs a 782s install forever, and it *hides* the failure:
a crashlooping app is indistinguishable from a slow one on the dashboard
card, which is the exact confusion ADR-0036 exists to remove. The agent just
edited the code and has `run_command`; restarting is its deliberate call.

`/tmp`, not `/workspace`: that is the task's git worktree, shared with the
worker pod, and a log file there lands in `git status` and gets committed.

### 3. An app is optional

Empty `start_cmd` means a sandbox-only pod — no app, no preview,
`run_command` unaffected — instead of a hard error.

**The readiness probe stays unconditional on `AppPort`.** Nothing will bind
it, so the pod stays NotReady forever, and that is the honest answer for a
sandbox with no app. Making the probe conditional was considered and rejected:
`PodState.AppReady` reads `ContainerStatuses[0].Ready`, so removing the probe
would make a no-app sandbox report `app_ready: true` and the dashboard would
render "ready" plus a preview link to a 502 — re-creating the reports-fine-
while-silently-broken failure ADR-0034/0036 exist to prevent.
`publishNotReadyAddresses` (ADR-0039) already keeps exec and code-server
routable regardless. The dashboard card gains one state instead:
`sandbox · no app`.

### 4. A terminal pod is not an existing session

`Failed`/`Succeeded` gets the same treatment `Terminating` already got: delete
and build fresh. Only the **pod** is deleted — Service/Middleware/IngressRoute
are create-if-absent, so the preview URL stays stable and Traefik doesn't
churn.

**No attempt counter.** Recreation is only ever triggered by an agent's own
`run_command`/`request_e2e_env` call; there is no server-side retry, so
nothing can spin on its own. The sidecar's backoff is the rate limiter. The
accepted ceiling: an agent retrying against a deterministically-broken pod
(bad init container, OOM, admission rejection) pays a delete + recreate each
time. Every recreate logs at `Warn` with the pod's phase and detail, so churn
is visible in Loki.

Accepted cost: deleting the corpse also deletes its `kubectl logs`. Loki
keeps them and `view_logs component=e2e` still works.

### 5. Waiting happens client-side, and status means something

`CreateE2ESession` returns `"requested"` (the truth) rather than `"running"`;
`e2eStatusFromPhase` answers `"exited"`/`"unknown"` instead of defaulting to
`"running"`. The sidecar's `run_command` replaces its single zero-delay retry
with a bounded backoff (~65s). `RequestE2eEnv` is idempotent, so re-calling it
each round is simultaneously the wait and the status probe — the final error
names the pod's real phase, the resolved recipe, and the `kill_env` escape
hatch, with no new RPC.

**Rejected: a bounded Running-wait inside `CreateE2ESession`.** It duplicates
the client's retry, forces a client deadline larger than itself, and would
hang every existing `grpcserver` test for the full timeout — the fake
clientset never sets a phase.

`CreateE2eSession` gets its own 5-minute client deadline; it can't reuse
`sessionCallTimeout` (2m) because the provisioner side legitimately includes a
2-minute `WaitForPodGone` plus `EnsureSharedInstance` waits.

### 6. Which profile the sandbox uses is a human's setting, with an approved override

`repos.e2e_profile` (dashboard-editable, `''` meaning the `"e2e"` convention,
mirroring `base_branch`'s existing convention in the same table). Resolution
order: agent override → repo column → `"e2e"`. agent-fleet is seeded to
`"lint"`.

The agent override finally wires `RequestE2EEnvRequest.profile` — a field core
already honored that no producer ever set, so ADR-0034's documented
agent-selectable profile was unreachable and every request resolved to the
literal name `"e2e"`. It goes through the same human-approval gate `startCmd`
uses, generalized rather than copied: which profile is used is a strictly
bigger lever than which command it runs, since it decides the toolchain and
the services too. ADR-0034's rule — a task branch must never silently change
what the provisioner creates — is preserved.

infra-bootstrap is deliberately **not** pointed at its `"worker"` profile:
that profile carries `cluster-access`, and granting the sandbox cluster reach
would break the strictly-less-privileged-than-the-worker premise ADR-0039
rests `run_command` being un-prompted on.

### 7. e2e pods are garbage-collected

A third `reconcile` pass, reusing `gcIdleSharedInstances`' shape and the
already-present-but-callerless `ListPodsByLabel`: terminal-phase pods and pods
older than `E2E_MAX_AGE_MS` (default 24h) are deleted via `DeleteAll`, with a
`SESSION_KIND_E2E` event reported so the deletion lands in the knowledge
journal — an agent whose sandbox vanished mid-session otherwise has no way to
learn why `run_command` started failing.

Age, not idleness: the provisioner holds no DB (ADR-0020 point 1), so there is
nowhere to record when a sandbox was last used. Coarser than the worker Jobs'
real done-signal, deliberately.

## Consequences

- The reported symptom is fixed at the root: no app failure can take the
  sandbox down, and every repo can start one.
- ADR-0039's two acknowledged gaps are closed: "a repo with no `e2e` profile
  fails at the first `run_command`" and "e2e pods are still never
  garbage-collected".
- ADR-0036's stay-alive goal is now actually achieved. Its readiness-probe
  decision is unchanged and re-affirmed here.
- The cached Playwright MCP client is dropped on any call error. Four paths
  now replace a pod under a live task (`kill_env`, terminating-recreate,
  failed-recreate, sweep) and only two ever called `DropClient` — the rest
  left the cache pointing at a corpse.
- `run_command`'s first call in a session now costs up to ~65s of waiting
  instead of returning a misleading error immediately.
- **There is a rolling-deploy window, and it self-heals.** If the new
  provisioner sends an empty `E2E_START_CMD` while the old e2e-runner image is
  still what gets pulled, that image's `E2E_START_CMD:?` guard fails the
  container instantly. It is bounded rather than sticky because
  `E2E_RUNNER_IMAGE` is a floating `:latest` (`k8s/provisioner/deployment.yaml`
  — an accepted v1 trade-off, see that directory's README), so
  `imagePullPolicy: Always` re-pulls, and decision 4 above means the failed pod
  is replaced on the agent's next `run_command` instead of being handed back
  forever. Worst case is one wasted sandbox creation on a repo with no
  profile, during the minutes between the two images landing. Still worth
  merging with the e2e-runner build green.

## Known gaps

- `sshd` daemonizes and is not supervised — a break-glass convenience, not a
  fleet dependency. Use code-server if it dies.
- A server that fails *immediately and always* now restart-loops every 5s
  instead of killing the pod. That is the intended trade (the pod stays
  debuggable), but it churns the log. The live candidate is
  `@playwright/mcp`'s `--port` flag, still unverified against the installed
  version — if it's wrong, expect a 5s restart line rather than silence.
  `supervise()` logs every restart with its label, so this is visible in Loki
  rather than mysterious.
- The sweep's age bound is fleet-wide, not per-repo. A legitimately long
  session hitting 24h loses its sandbox and gets a fresh one on the next
  `run_command`, which is a cold install.
- `run_command`'s description now lives in two places (the sidecar's and
  execmcp's own). They already drifted once — 15 minutes vs 5 — and are
  re-synced here.
