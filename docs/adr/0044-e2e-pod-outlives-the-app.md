# ADR-0044: The e2e pod is a sandbox that may also run an app

**Status:** Superseded by [0048](0048-one-session-one-pod-one-shared-home.md) — with one pod there is no separate lifetime to outlive. Its `wait -n`/PID-1 lesson survives as the agent's responsibility, documented in `fleet-shared/CLAUDE.md`
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

**The e2e pod is a sandbox that may also run an app.** Nine parts:

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

### 7. The browser tools are registered statically, from an embedded snapshot

`@playwright/mcp`'s tool set is a snapshot committed at
`sidecar/internal/mcpserver/playwright_tools.json` and registered in `New()`,
exactly as ADR-0039 did for `run_command` and for the same reason: **a tool
the agent cannot see is a tool that does not exist.**

Two independent bugs made browser automation unavailable for the fleet's
entire history, and both were silent because `ProxiedTools` collapses any
failure into a `nil` list at log level `Info`:

**a. Every provisioner call got a 403.** `@playwright/mcp` defaults
`--allowed-hosts` to the host it is bound to and rejects every other `Host`
header. Measured against a real image:

| `Host` header | Response |
|---|---|
| `localhost:8931` | `200` |
| `e2e-abc.agent-fleet.svc.cluster.local:8931` (what the provisioner sends) | `403 Access is only allowed at localhost:8931` |
| the same, with `--allowed-hosts '*'` | `200` |

This is why ADR-0012/0036/0039 kept carrying an unverified risk about the
`--port` flag. The flag was always fine; the Host check was the problem.
Passing `'*'` disables a DNS-rebinding guard whose threat model — a browser on
a user's machine reaching a localhost server — cannot occur here:
`k8s/provisioner/networkpolicy.yaml` admits `:8931` from the provisioner pod
**only**, and the port has no IngressRoute. Cilium enforcing L3 is a strictly
stronger boundary than a `Host` string match. If `:8931` ever gains an
IngressRoute, narrow this to the pod's own service DNS name.

**b. Runtime discovery lost the race, three ways.** Both registration sites
fired immediately after `RequestE2eEnv` returned — i.e. when the pod *object*
existed, not when it served. Measured: `execmcp` binds at **t+2s**,
`@playwright/mcp` at **t+5s**. So (1) discovery landed in that gap and got
connection-refused; (2) `run_command` breaks out of its retry loop the moment
`execmcp` answers, so no later attempt happened; (3) on a resumed session with
a warm pod, `run_command`'s first call succeeds, so the provisioning branch —
the only thing that registered anything — was never reached at all.

Static registration removes the race and, with it, the dependency on the
client honoring `notifications/tools/list_changed` — a risk ADR-0012 flagged
and nobody ever verified. The tools now proxy to a pod that may not exist yet;
that is deliberate, and a loud failure beats an invisible tool.

**c. The browser itself was never installed.** Only surfaced by actually
driving a `browser_navigate` — every cheaper check (port bound, MCP
`initialize`, `tools/list`) passed:

| Configuration | Error |
|---|---|
| default channel | `Chromium distribution 'chrome' is not found at /opt/google/chrome/chrome` — it wants **branded** Chrome, which the image neither ships nor should |
| `--browser chromium` alone | `Browser "chrome-for-testing" is not installed; expected .../chromium-1237/...` — the image had `chromium-1234` |

`@playwright/mcp` bundles a different `playwright-core` than the global
`playwright` package, so it resolves a different browser build number. Fixed
by passing `--browser chromium` **and** adding
`bunx @playwright/mcp install-browser chrome-for-testing` to the Dockerfile,
which lets @playwright/mcp resolve its own build rather than pinning the two
packages' `playwright-core` versions by hand — that would break silently on
the next bump instead of loudly at build time. ~110MB.

Cost: the snapshot drifts from the installed `@playwright/mcp`. Bounded and
one-directional — a tool that disappeared upstream fails loudly at the real
server on first call, a new one is merely invisible until someone refreshes
the file. `sidecar/internal/mcpserver/playwright_tools_test.go` pins that it
parses, is populated, carries real schemas, contains the core browser tools,
and never contains `run_command`.

**Rejected: retrying discovery on every `run_command` until it sticks.** It
self-heals cold start and resumed sessions, and it is fewer lines — but it
still rests on `list_changed` being honored, which is precisely the unverified
assumption that let this rot undetected. Static registration answers the
question instead of re-betting on it.

### 8. A human can drive the sandbox, not just the agent

The dashboard could Kill an e2e env but never start one: creating a sandbox
was reachable only by asking the agent to call `request_e2e_env` — a strange
dependency, since the sandbox is exactly what a human wants in order to
inspect a broken preview themselves. The E2E card in the detail view's panel
column gains a **Manage** drawer with `Start` / `Restart app` / `Stop` /
`Recreate pod`, the code-server link, and the app log.

**Restart app and Recreate pod are separate buttons on purpose.** Restarting
re-runs the start command inside the live pod (seconds, keeps the warm
dependency cache); recreating throws that cache away for a 10+ minute cold
install. One "Restart" button covering both is how a human pays that install
to fix a dev server that just needed rebooting — so recreate is the only
action behind a confirm, and the copy names the cost.

The restart logic lives in the image as `e2e-restart-app`, not in the
dashboard: the part that actually matters is signalling the app's whole
process group, so a dev server's children don't survive holding `$PORT` and
make the next start fail with `EADDRINUSE`. Putting it next to the thing it
restarts means the agent invokes the same command (`fleet-shared/CLAUDE.md`
tells it to) instead of each caller hand-composing a shell line.

The log comes from `/tmp/e2e-app.log` via the existing `run_command`
passthrough — no new ProvisionerService RPC — and is a drawer rather than more
rows on the card because at the panel column's 266px every log line wraps
three times.

Two bugs surfaced while building it, both from re-deriving on the client what
the server already knows:

- **"Open code-server" opened the app.** It was wired to `preview_url` (the
  app root); code-server is served at the `/code` prefix (ADR-0038). It had
  never once opened the IDE. `code_server_url` is now constructed next to the
  route that serves it and travels on the wire.
- **The e2e card reported the wrong profile.** `GetE2EStatus` had its own copy
  of the hardcoded `"e2e"` lookup, so for any repo whose `e2e_profile` points
  elsewhere it named a profile the pod wasn't built from — and computed a
  spurious `start_cmd_overridden` badge off it. Both callers now go through
  `core/internal/e2erecipe`, which exists precisely because that rule had
  already been copied and had already drifted.

### 9. e2e pods are garbage-collected

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
  debuggable), but it churns the log. `supervise()` logs every restart with
  its label, so this is visible in Loki rather than mysterious.

## Verified against a real container

Built `e2e-runner/Dockerfile` and exercised the image directly (the
provisioner-side halves — empty `start_cmd` accepted, terminal pod replaced,
the GC sweep — are covered by Go tests against the fake clientset):

| Case | Result |
|---|---|
| `start_cmd` exits non-zero (`exit 7`) | Container **stayed Running**; `/tmp/e2e-app.log` ended with `--- e2e app command exited with status 7 ---`; execmcp, code-server, Playwright and sshd all still bound. This is the regression that motivated the ADR. |
| No `E2E_START_CMD` at all | Container ran; `run_command` fully functional; log recorded the sandbox-only state. (`go` absent, `bun` present — correct: toolchains come from the provisioner's ingredient init containers, not the image, which is exactly what the `e2e_profile` column exists to select.) |
| A real serving app | App served on `:3000`, `run_command` worked concurrently against it. |
| `pkill -9 execmcp` | Container survived; `supervise()` logged `execmcp exited (137), restarting in 5s`; the listener came back and served. |
| `run_command 'sleep 300 & echo ok'` | Returned in **1.025s** — precisely `cmd.WaitDelay`, confirming both that the inherited pipe really was held open and that the fix releases it. Before this change the same call blocked for the full 15-minute `commandTimeout`. |
| `browser_navigate` through the real Playwright MCP, using the in-cluster `Host` header, on a clean container off the final image | **Chromium launched, page loaded, snapshot produced.** The first time browser automation has worked in this fleet. |
| snapshot vs. live `tools/list` | 24 tools, exact match, no drift. |

Re-verified against a **real provisioner and real pods** in a kind cluster
(`/kind-local`), driving the dashboard RPCs rather than the unit-test fakes:

| Case | Result |
|---|---|
| `StartE2e` on a repo whose `e2e_profile` is `preview` | Resolved that profile (not the hardcoded `"e2e"`), created a real pod, returned `"requested"` — the truthful phase, not the old `"running"` lie. |
| `GetE2eStatus` | `codeServerUrl` came back as the `/code/` route, `profileName: preview`, `appReady: true`. |
| `GetE2eAppLog` | Read `/tmp/e2e-app.log` out of the live pod through the `run_command` passthrough. |
| `RestartE2eApp` | Replaced the app's process group (PIDs 36/84 → 103/110), port rebound with no `EADDRINUSE`, and the pod's own `restartCount` stayed **0** — the pod never bounced. |
| App killed outright, then `RestartE2eApp` | Pod stayed `Running` with `restartCount: 0` and the sandbox stayed usable while the app was dead; one RPC brought it back. |
| A repo with **no profile at all** | Pod created and usable — `0/1 Running`, since nothing binds the app port and NotReady is the honest answer. This is the case that used to fail pod creation outright. |
| `KillE2e`, then `StartE2e` (the drawer's Recreate) | Kill returned `killed: true`; the immediately-following create waited out the terminating pod (31s in `WaitForPodGone`) and built a fresh one that served. |

Not reproducible live: a lingering `Failed` pod. `kill -9 1` is ignored inside
the container's PID namespace, and forcing the phase via the status subresource
made the API server delete the pod outright. That the corpse state is now hard
to even *produce* is the point of decision 1; the replacement path is covered
by unit tests against both terminal phases instead.

**Resolved a long-standing unverified risk, and found the real one behind
it:** `@playwright/mcp --port` *does* work — `:8931` was bound in every run,
closing the open question ADR-0012/0036/0039 all carried. But binding is not
reachability: the server then 403'd every request whose `Host` header was not
its own bind address, which is every request the provisioner has ever made.
See decision 7a.
- The sweep's age bound is fleet-wide, not per-repo. A legitimately long
  session hitting 24h loses its sandbox and gets a fresh one on the next
  `run_command`, which is a cold install.
- `run_command`'s description now lives in two places (the sidecar's and
  execmcp's own). They already drifted once — 15 minutes vs 5 — and are
  re-synced here.
- The Playwright tool snapshot is a committed file, so bumping
  `@playwright/mcp` needs a manual refresh (decision 7). Nothing detects the
  drift automatically; the tests only pin that the file is well-formed and
  plausible.
- **`app_ready` is slow to go false.** The readiness probe's
  `failureThreshold: 120` × `periodSeconds: 10` is ~20 minutes, deliberately,
  so a 782s cold install isn't reported as a broken app (ADR-0036). The
  consequence, confirmed live in kind: after an app dies, the card keeps
  saying "ready" for up to 20 minutes. The app log now answers the question
  the badge can't, which is the mitigation — lowering the threshold would
  trade a slow-true for a fast-wrong on every cold start.
