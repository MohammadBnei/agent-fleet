# End-to-End Test Log

This file records verified end-to-end test runs of agent-fleet. Two kinds count,
and they prove different things — a run should say which it was:

- **kind-local golden path** — the full dispatch path (session → worker Job → PR)
  against a disposable kind cluster. Proves the fleet *works*.
- **dashboard local stack** — throwaway Postgres + a stub provisioner + `core` +
  the dashboard dev server, driven by Playwright. Proves the UI works. The stub
  is not optional any more: since `adr/0048` the first message provisions, and
  `PostMessage` propagates `CreateWorkerPod`'s error, so with nothing listening
  the message is never appended. There is still no Garage, so `ListFiles` is
  Unavailable.
- **cluster scratch namespace** — the real `ukubi-cluster`, in a disposable
  namespace. The only run that exercises anything requiring a real provisioner,
  a real shared PVC, or real Loki.

## What Constitutes a Successful Test

A successful **golden-path** test verifies the complete flow from task creation to
PR:

1. Kind cluster created successfully
2. All images built and loaded (core, provisioner, sidecar, worker)
3. All components deployed and ready (postgres, core, provisioner)
4. Session created, and a **first message sent** — nothing is claimed and
   nothing dispatches on its own; the message is what provisions (`adr/0048`)
5. Worker Job created and completed
6. PR opened on target repository

See [`local/kind/README.md`](../local/kind/README.md) and the `/kind-local` skill
for the full procedure; `/dashboard-e2e` covers the dashboard local stack.

## Cluster scratch-namespace runs

Anything that needs a real provisioner (a session PVC actually being created and
bound, `expose()` returning a reachable URL), real Loki (the log drawer), or a
real PR can only be verified on the cluster. Run it in a **disposable
namespace**, never against the live one, and tear it down afterwards:

- Namespace `agent-fleet-scratch`, applied with `kubectl` and **never** added to
  `gitops/apps/registry.yaml` — ArgoCD would otherwise prune or fight it. This is
  a deliberate, documented GitOps bypass with a teardown, not a new environment.
  Namespace/pod ops only: kubespray/ansible/pigsty stay the parent repo's
  boundary.
- **Its own shared PVC.** The production one holds the clone cache and every
  session's `claude-home`; a scratch provisioner pointed at it could clone or
  sweep against real work. (Per-session PVCs are created per session, so those
  take care of themselves.)
- **Its own database**, not `agentfleetdb`, with `db/migrations/` applied to it.
- **No `DISCORD_BOT_TOKEN`** — a second core on the real token double-posts into
  the live channel. Core falls back to `noopNotifier`.
- **Its own ServiceAccount + namespaced Role/RoleBinding** copied from
  `k8s/provisioner/`; the provisioner's RBAC is namespaced by design, so it cannot
  reach outside the scratch namespace.
- **Released image tags only.** A PR build runs `push: false`, so a manifest
  naming a PR-only tag `ImagePullBackOff`s. And `kubectl apply
  --dry-run=server` validates a Deployment naming a non-existent ServiceAccount
  perfectly and then never schedules — look at pods, not at sync status.

Teardown: `kubectl delete namespace agent-fleet-scratch` (takes the PVCs, RBAC
and any orphaned worker pods with it), drop the scratch database, close any
throwaway PR, and confirm nothing was left in the production namespace or on the
shared PVC.

## Test History

Entries are kept as written on the day. Where a run verified something that has
since been deleted, the note says so rather than being edited — what a run
proved *at the time* is the record; what still exists is
[`ARCHITECTURE.md`](ARCHITECTURE.md)'s job.

| Date       | Kind | Verified By | Notes |
|------------|------|-------------|-------|
| 2026-08-06 | kind-local | Agent | Initial log creation; establishing baseline for future test runs |
| 2026-08-10 | kind-local | Agent | Verified fleet-shared skills sync (docs/adr/0032) in kind-local after fixing mirror-path bug |
| 2026-08-12 | dashboard local stack | Agent | Console rewrite ([adr/0042](adr/0042-console-rewrite.md)). 9 Playwright specs at 1440×900 **and** 390×844: both themes on every view + theme surviving reload; a blocked session denied **from the list** with the reason arriving; all three density modes on both form factors incl. the deliberate desktop/mobile disagreement; the mobile decision dock above the composer; the tab bar reaching every view. Overflow checked by measuring rects (clip-aware) + a hard `scrollWidth <= 390` gate. Found two defects review missed: the load-error modal trapped navigation (now `InlineError`), and `body` never took the theme. *(Three of these specs covered surfaces `adr/0048` later deleted — the decision spine, `RetryTask` on a `failed_permanently` task, and the worktrees screen.)* |
| 2026-08-15 | dashboard local stack | Agent | First run against the post-`adr/0048` console, and the run that made the skill able to run at all: an unreachable provisioner now fails `PostMessage` outright, so it needed a **stub `ProvisionerService`** (`CreateWorkerPod`/`TearDownSession`/`ListWorkerPods`, the last echoing its pods back or core's 60s reconcile tears the session down mid-run). 10 specs, green twice from a re-applied seed — the idempotency that matters, since answering a permission consumes it. Covered: create an **empty** session (row visible, **no pod**, empty transcript); create **with** a first message (pod provisioned, message in the transcript **verbatim**); a snippet inserting editable text into the message box; a permission answered from `DecisionInline` in the list **and** from `DecisionDock` in the detail; a legacy `?task=` link still resolving; the three-way DENSITY control; archive; mobile at 390×844 with no horizontal scroll; zero `pageerror`s. **Found two live bugs CI was green on:** creating a session from the dashboard did nothing at all (the instruction went to `description`, which the agent never reads — shipped in v3.0.0), and one NULL `sessions.description` failed the scan for the *entire* `List` query, emptying the session list, the reconcile loop's view and the live-state gauge together. **Not covered — needs a cluster run:** a real PVC binding, `expose()` returning a reachable URL, the log drawer against real Loki, and a scheduled audit producing a real session. |
