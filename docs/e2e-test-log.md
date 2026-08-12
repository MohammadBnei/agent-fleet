# End-to-End Test Log

This file records verified end-to-end test runs of agent-fleet. Two kinds count,
and they prove different things — a run should say which it was:

- **kind-local golden path** — the full dispatch path (task → worker Job → PR)
  against a disposable kind cluster. Proves the fleet *works*.
- **dashboard local stack** — throwaway Postgres + `core` + the dashboard dev
  server, driven by Playwright. Proves the UI works, but with **no provisioner and
  no Garage**, so `ListWorktrees`, `GetE2eStatus` and `ListFiles` are Unavailable.
- **cluster scratch namespace** — the real `ukubi-cluster`, in a disposable
  namespace. The only run that exercises anything requiring a real provisioner,
  a real shared PVC, or real Loki.

## What Constitutes a Successful Test

A successful **golden-path** test verifies the complete flow from task creation to
PR:

1. Kind cluster created successfully
2. All images built and loaded (core, provisioner, sidecar, worker)
3. All components deployed and ready (postgres, core, provisioner)
4. Task created and claimed by core
5. Worker Job dispatched and completed
6. PR opened on target repository

See [`local/kind/README.md`](../local/kind/README.md) and the `/kind-local` skill
for the full procedure; `/dashboard-e2e` covers the dashboard local stack.

## Cluster scratch-namespace runs

Anything that needs a real provisioner (worktree `path`/`dirty_files`/
`size_bytes`, the PVC capacity meter), real Loki (the log drawer), or a real PR
can only be verified on the cluster. Run it in a **disposable namespace**, never
against the live one, and tear it down afterwards:

- Namespace `agent-fleet-scratch`, applied with `kubectl` and **never** added to
  `gitops/apps/registry.yaml` — ArgoCD would otherwise prune or fight it. This is
  a deliberate, documented GitOps bypass with a teardown, not a new environment.
  Namespace/pod ops only: kubespray/ansible/pigsty stay the parent repo's
  boundary.
- **Its own PVC.** The production workspace PVC is shared RWX and holds live
  worktrees; a scratch provisioner pointed at it could clone, sweep or
  `git worktree remove` against real work.
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

Teardown: `kubectl delete namespace agent-fleet-scratch` (takes the PVC, RBAC and
any orphaned worker/e2e pods with it), drop the scratch database, close any
throwaway PR, and confirm nothing was left in the production namespace or on the
shared PVC.

## Test History

| Date       | Kind | Verified By | Notes |
|------------|------|-------------|-------|
| 2026-08-06 | kind-local | Agent | Initial log creation; establishing baseline for future test runs |
| 2026-08-10 | kind-local | Agent | Verified fleet-shared skills sync (docs/adr/0032) in kind-local after fixing mirror-path bug |
| 2026-08-12 | dashboard local stack | Agent | Console rewrite ([adr/0042](adr/0042-console-rewrite.md)). 9 Playwright specs at 1440×900 **and** 390×844: both themes on all five views + theme surviving reload; a blocked session denied **from the list** with the reason reaching the decision spine; `RetryTask` on a real `failed_permanently` task; all three density modes on both form factors incl. the deliberate desktop/mobile disagreement; spine-jump landing on the real entry; the mobile decision dock above the composer; the tab bar reaching every view. Overflow checked by measuring rects (clip-aware) + a hard `scrollWidth <= 390` gate. Found two defects review missed: the load-error modal trapped navigation (now `InlineError`), and `body` never took the theme. **Not covered — needs a cluster run:** worktree `path`/`dirty_files`/`size_bytes`, the PVC meter, the log drawer against real Loki, and `RunScheduledAuditNow` producing a real dispatched run. |
