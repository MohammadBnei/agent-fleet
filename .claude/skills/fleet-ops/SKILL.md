---
name: fleet-ops
description: Onboard a new target repo, walk the release/deploy pipeline, or inspect the live sessions for agent-fleet. Use when the user wants to add a new repo to the fleet, deploy a release, or check which sessions are running/stuck.
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

Three recurring ops jobs. See `docs/ARCHITECTURE.md` for the full topology
this assumes — there's no per-repo Deployment or PVC; the provisioner spawns
a single-shot, two-container worker pod per *warm* on demand, and a session
with no message has no pod at all (`docs/adr/0048` §1).

## 1. Onboard a new target repo

No new k8s manifest, no new Deployment, no redeploy — repo config lives in
the dashboard-editable `repos` table (`docs/adr/0028`), not Go source:

1. **Dashboard → "manage repos"** — add an entry: name, git URL, base
   branch (only needs setting if the repo doesn't develop off `main` — see
   `vos-monolith`'s row, base branch `dev`). This writes straight to the
   `repos` table (`core/internal/repos.Store`), and the next session on that
   repo reads the new row when it provisions — no core restart. Direct SQL
   (`INSERT INTO repos (name, url, base_branch) VALUES (...)`) works too if
   the dashboard isn't reachable.
2. **Infisical** — confirm the target repo's bot GitHub account
   (`GH_TOKEN`, shared by the provisioner's clone/fetch and the worker's
   push/PR) has collaborator access to open PRs there. The rest of the
   secrets (`CLAUDE_CODE_OAUTH_TOKEN`, `AGENTFLEET_DB_*`) are already
   shared across the `agent-fleet-nygh` Infisical project.
3. **Decide the repo's image and cluster access** — the other two columns in
   the same "manage repos" row (`ManageReposModal.tsx`):

   - `image` — the container image this repo's sessions run. Blank means the
     fleet's default worker image, which carries bun, Go, git, `gh` and the
     Claude Code CLI (see `worker/Dockerfile` for the real list — it does
     *not* include `golangci-lint` or `buf`). Set it only when the repo needs
     a toolchain the default lacks. This one column replaced the old
     `repo_profiles`/`repo_profile_tools` rows and their four toolchain
     ingredients (`docs/adr/0048` §6): there is no sandbox pod any more, so
     the agent builds and tests in its own pod's `Bash` and "which toolchain"
     is just "which image".
   - `cluster_access` — whether this repo's sessions get the `kubectl` shim
     that RPCs to thot-executor (`docs/adr/0037`). A privilege grant, not a
     toolchain; the pod still holds zero Kubernetes credentials either way.
     Currently true for `infra-bootstrap` alone.

   There is nothing else to configure. No profile, no start command, no e2e
   preview, no `run_command` — `docs/adr/0048` §6 deleted all of it along
   with the sandbox pod.

### Bumping the browser toolchain

The agent's browser tools come from `@playwright/mcp` running as a **stdio
MCP server inside the worker container itself** (`worker/src/session.ts`'s
`playwrightMcpServer()`), so there is no snapshot to refresh and no tool list
to fetch — the SDK discovers them over the pipe it owns. The committed
`sidecar/internal/mcpserver/playwright_tools.json` and the `e2e-runner`
image that served it both died with `docs/adr/0048` §6.

What does need care is the pair of pinned versions in `worker/Dockerfile`:

```dockerfile
ARG PLAYWRIGHT_VERSION=1.62.1
ARG PLAYWRIGHT_MCP_VERSION=0.0.79
```

The browser **builds** live on the shared PVC, not in the image
(`docs/adr/0051`), installed by the `agent-fleet-browser-cache` Job that
`EnsureBrowserCache` fires on every provisioner start
(`provisioner/internal/k8s/browsercache.go`). Bumping either ARG changes
which build number the installers resolve; the Job is recreated whenever it
references a different image, so a deliberate bump repairs the cache on the
next provisioner start. It also recreates a Failed one, so a broken cache
retries instead of sitting dead.

Both versions must move together with care: `@playwright/mcp` bundles a
*different* `playwright-core` than `playwright` does and resolves a different
build number, which is why the Job runs both installers and why the server is
launched with `--browser chromium` (`docs/adr/0044`). Browser automation was
dead for the fleet's entire history behind that.

**Verify a bump by driving a real `browser_navigate` from a session.** A
populated `/browsers` and a launching MCP server are different claims — the
mismatch fails at launch, never at `ls`.

No `infra-bootstrap` edit needed (that repo only knows about `core`'s and
`provisioner`'s own Applications, not individual target repos). Unlike the
old code-review-gated flow, a dashboard-added repo is live immediately —
weigh that before adding one on a whim, since it becomes selectable in the
dashboard's new-session dialog fleet-wide right away.

## 2. Release / deploy walkthrough

```
git push origin <tag>              # any tag push triggers docker.yml
  → changes (ubuntu-latest, PR only): paths-filter -> the component list
  → build-push (ONE job, on [self-hosted, ukubi-build] — the build-runner LXC):
      loops core provisioner sidecar executor worker migration, and for each
      runs `sudo buildah bud` then pushes <version> + latest to
      registry.bnei.lan:5000 (ukubi's own Zot, infra-bootstrap ADR-0034).
      One job, not one per image: the runner executes one job at a time, so a
      matrix would serialize anyway and each leg's cleanup would evict the
      shared golang:1.26 / oven/bun:1-slim cache. On a PR only the changed
      components build, and nothing is pushed.
  → deploy (on tag push only — NOT on workflow_dispatch, needs build-push):
      sed-bumps k8s/*.yaml's `tag: "..."` (Helm-values shape) and
      every `image: repo:tag` in k8s/provisioner/*.yaml (plain-manifest
      shape, scoped to registry.bnei.lan:5000/agent-fleet-{core,provisioner,
      worker,sidecar}), then greps to prove the bump landed, commits to main
      (uses the default GITHUB_TOKEN — deliberately doesn't re-trigger release.yml)
  → ArgoCD: core (two-source Application, chart from infra-bootstrap +
      k8s/core.yaml here) and provisioner (standalone plain-manifest
      Application, k8s/provisioner/ here) both pick up their new tags and sync
```

`release.yml` is separate and unrelated to the above — it runs
`release-it` on ordinary pushes to `main` to bump the version/CHANGELOG
from Conventional Commits. It does not build or deploy anything.

Check current live tags: `grep 'tag:' k8s/core.yaml; grep 'image:'
k8s/provisioner/deployment.yaml`. Check what actually reached the registry —
a green build is not the same claim, and anonymous read means no auth:

```bash
curl -s http://registry.bnei.lan:5000/v2/_catalog
curl -s http://registry.bnei.lan:5000/v2/agent-fleet-worker/tags/list
```

Retention keeps only the **last 5 tags per image** plus `latest`
(infra-bootstrap `gitops/platform/values/zot/values.yaml`), so a rollback
deeper than 5 releases needs the image rebuilt — `workflow_dispatch` after
checking out the older tag. Runner/build/disk problems on the box itself go
through infra-bootstrap's `/build-runner-ops`.

Check what's actually running:
`kubectl get pods -n agent-fleet -o wide` — expect `core` and
`provisioner` Deployments, zero-or-more two-container
`worker-<shortSessionId>` Pods (one per warm session — see
`WorkerResourceName`/`shortID` in `provisioner/internal/k8s/names.go`), and
a completed `agent-fleet-browser-cache` Job (via `/k8s-ops` in
`infra-bootstrap` if you need cluster access set up).

Never push a tag or edit `k8s/core.yaml`/`k8s/provisioner/*.yaml`'s
pinned versions without confirming — this redeploys the two persistent
services and changes what image every *future* worker/sidecar pod spawns
with (in-flight pods keep running their already-pinned image).

## 3. Inspect the live sessions

There is **no queue** — no `tasks` table, no status enum, no lease renewal,
no retry counter (`docs/adr/0048` §2). A session is the durable unit and a
pod is ephemeral compute attached to it; what a session "is doing" is
`pod_phase`, which core's 60s reconcile loop writes from what Kubernetes
actually reports (`core/internal/sessions/reconcile.go`).

```sql
-- what has a pod right now
SELECT id, repo, pod_phase, permission_mode, last_entry_type, last_active_at
FROM sessions
WHERE archived_at IS NULL AND pod_phase IS NOT NULL
ORDER BY last_active_at DESC NULLS LAST;

-- the live count MAX_LIVE_SESSIONS is checked against. Keep this list in
-- step with `livePhases` in core/internal/sessions/store.go, which is the
-- real one. A session stuck non-terminal costs one permanent slot and
-- nothing errors until the cap is hit — which is why the check for that
-- class of bug is "run six sessions and open a seventh".
SELECT count(*) FROM sessions
WHERE archived_at IS NULL AND pod_phase IN (
  'POD_PHASE_PROVISIONING', 'POD_PHASE_CREATED',
  'POD_PHASE_SCHEDULED',    'POD_PHASE_RUNNING'
);

-- recent history for one repo
SELECT id, title, pod_phase, last_error, created_at, updated_at
FROM sessions WHERE repo = '<repo>' ORDER BY created_at DESC LIMIT 20;

-- what a session's worker/provisioner actually did
SELECT event_type, payload, created_at
FROM knowledge_journal
WHERE payload->>'sessionId' = '<sessionId>'
ORDER BY created_at;

-- currently configured repos (docs/adr/0028)
SELECT name, url, base_branch, image, cluster_access FROM repos ORDER BY name;
```

Connect via the `AGENTFLEET_DB_*` credentials (Infisical, `agent-fleet-nygh`
project) — `core` is the **only** component with these
(`docs/adr/0020` point 1); `provisioner`/`sidecar`/`worker` hold none.
`knowledge_journal` also carries provisioner-reported pod-lifecycle events,
pushed live over gRPC and journaled by `core`'s `ReportPodEvents` — useful
for seeing whether a stuck session even got a pod at all. `event_type` is
`"pod." + PodPhase.String()`, so the values are
`pod.POD_PHASE_CREATED`/`_SCHEDULED`/`_RUNNING`/`_CRASHED`/`_TERMINATED` —
the full enum name, not a lowercased one. For diagnosing *why* a specific
session is stuck rather than just listing state, use `/fleet-debug` instead.
