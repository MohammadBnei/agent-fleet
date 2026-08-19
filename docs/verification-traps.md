# agent-fleet — verification traps

Green CI is necessary, not sufficient. Every failure below was **silent**: the
check passed, or was not run, or measured the wrong thing. Each one shipped a
real bug, and most now have a named guard test.

`agent-fleet/CLAUDE.md` carries the one-line version of this list — enough to
recognise the shape of a trap while working. This file is the full account:
what broke, why nothing caught it, and what guards it now. Read it when a
check passes and you are not convinced, or before trusting a green run on
anything in the deploy, CI, permission or pod-lifecycle paths.

The first five each shipped a bug on 2026-08-11; the last had been shipping
since the feature was written.

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
