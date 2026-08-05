---
name: fleet-ops
description: Onboard a new target repo, walk the release/deploy pipeline, or inspect the live task queue for agent-fleet. Use when the user wants to add a new repo to the fleet, deploy a release, or check what tasks are running/stuck.
user-invocable: true
allowed-tools:
  - Read
  - Edit
  - Bash(git diff *)
  - Bash(git status *)
  - Bash(git log *)
  - Bash(psql *)
  - Bash(gh *)
---

# /fleet-ops — operate the agent-fleet deployment

Three recurring ops tasks. See `docs/ARCHITECTURE.md` for the full topology
this assumes — as of `docs/adr/0019`–`0021` there's no per-repo Deployment
or PVC anymore; the provisioner spawns a single-shot, two-container worker
pod per task on demand.

## 1. Onboard a new target repo

No new k8s manifest, no new Deployment — just a config entry plus a
GitHub-side grant:

1. **`core/internal/tasks/store.go`'s `KnownRepos` map** — add an entry:
   `{URL: "https://github.com/...", BaseBranch: "..."}`. `BaseBranch` only
   needs setting if the repo doesn't develop off `main` (see
   `vos-monolith`'s entry — its default branch is `dev`). Without this,
   `/task`'s dropdown won't offer the repo (`core/internal/discord/
   commands.go`'s `repoChoices()` reads this map) and the dispatch loop
   will refuse to dispatch a claimed task for it (`core/internal/dispatch/
   loop.go`'s `tick()` checks `KnownRepos` before calling
   `CreateWorkerPod`).
2. **Infisical** — confirm the target repo's bot GitHub account
   (`GH_TOKEN`, shared by the provisioner's clone/fetch and the worker's
   push/PR) has collaborator access to open PRs there. The rest of the
   secrets (`CLAUDE_CODE_OAUTH_TOKEN`, `AGENTFLEET_DB_*`) are already
   shared across the `agent-fleet-nygh` Infisical project.
3. **Confirm the repo's own test/build command** if it needs anything
   beyond what the agent can infer — there's no per-repo e2e start-command
   config file, just `provisioner/internal/k8s/names.go`'s `StartCmdFor`
   (a small `switch`, env-overridable via `E2E_START_CMD_<REPO>`) for the
   on-demand e2e-preview pod's app-start command, if this repo will use
   that feature.

This is a one-file, one-repo change — no `infra-bootstrap` edit needed
(that repo only knows about `core`'s and `provisioner`'s own
Applications, not individual target repos). Confirm with Mohammad before
merging, since it changes what `/task` will accept fleet-wide.

## 2. Release / deploy walkthrough

```
git push origin <tag>              # any tag push triggers docker.yml
  → build-push (matrix: worker): Bun image, push to Docker Hub, Trivy scan
  → build-push-go (matrix: core, provisioner, sidecar): Go images, same
  → deploy (on push only, needs both build jobs):
      sed-bumps k8s/core.yaml's `tag: "..."` (Helm-values shape) and
      every `image: repo:tag` in k8s/provisioner/*.yaml (plain-manifest
      shape, scoped to the four mohammaddocker/agent-fleet-{core,
      provisioner,worker,sidecar} images — e2e-runner's floating :latest
      is deliberately excluded), commits to main
      (uses the default GITHUB_TOKEN — deliberately doesn't re-trigger release.yml)
  → ArgoCD: core (two-source Application, chart from infra-bootstrap +
      k8s/core.yaml here) and provisioner (standalone plain-manifest
      Application, k8s/provisioner/ here) both pick up their new tags and sync
```

`release.yml` is separate and unrelated to the above — it runs
`release-it` on ordinary pushes to `main` to bump the version/CHANGELOG
from Conventional Commits. It does not build or deploy anything.

Check current live tags: `grep 'tag:' k8s/core.yaml; grep 'image:'
k8s/provisioner/deployment.yaml`. Check what's actually running:
`kubectl get pods -n agent-fleet -o wide` — expect `core` and
`provisioner` Deployments plus zero-or-more `worker-<taskid>` Pods, one
per in-flight task, each two-container (via `/k8s-ops` in
`infra-bootstrap` if you need cluster access set up).

Never push a tag or edit `k8s/core.yaml`/`k8s/provisioner/*.yaml`'s
pinned versions without confirming — this redeploys the two persistent
services and changes what image every *future* worker/sidecar pod spawns
with (in-flight pods keep running their already-pinned image).

## 3. Inspect the live task queue

```sql
-- what's running/stuck right now
SELECT id, repo, status, claimed_by, heartbeat_at, created_at, updated_at
FROM tasks
WHERE status NOT IN ('done', 'cancelled')
ORDER BY created_at;

-- recent history for one repo
SELECT id, status, pr_url, created_at, updated_at
FROM tasks WHERE repo = '<repo>' ORDER BY created_at DESC LIMIT 20;

-- what a task's worker/provisioner actually did
SELECT event_type, payload, created_at
FROM knowledge_journal
WHERE payload->>'taskId' = '<taskId>'
ORDER BY created_at;
```

Connect via the `AGENTFLEET_DB_*` credentials (Infisical, `agent-fleet-nygh`
project) — `core` is the **only** component with these
(`docs/adr/0020` point 1); `provisioner`/`sidecar`/`worker` hold none.
`knowledge_journal` now also carries provisioner-reported pod-lifecycle
events (`event_type` like `pod.created`/`pod.scheduled`/`pod.running`/
`pod.crashed`/`pod.terminated`), pushed live over gRPC and journaled by
`core`'s `ReportPodEvents` — useful for seeing whether a stuck task even
got a pod at all. For diagnosing *why* a specific task is stuck rather
than just listing state, use `/fleet-debug` instead.
