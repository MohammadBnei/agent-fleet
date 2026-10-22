---
name: fleet-ops
description: Onboard a new target repo (install the ukubi-fleet GitHub App), walk the release/deploy pipeline, or inspect the live sessions for agent-fleet. Use when the user wants to add a new repo to the fleet, deploy a release, or check which sessions are running/stuck.
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

A repo is in the fleet while the GitHub App `ukubi-fleet` is installed on it
by an allowlisted account (`docs/adr/0075` §4, amending `0074`). No
manifest, no redeploy, no console form:

1. **GitHub**: install `ukubi-fleet` on the repo. Both installations
   (MohammadBnei, opus-tech-hq; `FLEET_GITHUB_ACCOUNTS` in
   `k8s/provisioner/deployment.yaml`) are set to "All repositories", so a new
   repo under either account joins on its own. Core's 60s tick inserts the row
   (installations are re-listed every 10 min, so a new account takes up to
   that). The base branch is the repo's GitHub default branch. An installation
   by any other account is ignored and logged; remove it by hand. The repo
   arrives **active**; to keep the fleet off it, switch it off in the
   console's Settings → Repos (an inactive repo starts no new session; open
   ones keep working).
2. **Only if the defaults are wrong**: add an entry to infra-bootstrap's
   `gitops/bootstrap/fleet-repo-overrides.yaml`, keyed by `owner/repo` with
   the repo's GitHub `id` (`gh api repos/<owner>/<repo> --jq .id`):

   - `name`: the fleet name, when the GitHub name collides with another
     repo's or definitions already use a different one.
   - `baseBranch`: when the fleet should not work off the default branch.
   - `image`: the container image this repo's sessions run. Blank means the
     fleet's default worker image, which carries bun, Go, git, `gh` and the
     Claude Code CLI (see `worker/Dockerfile` for the real list — it does
     *not* include `golangci-lint` or `buf`). Set it only when the repo needs
     a toolchain the default lacks (`docs/adr/0048` §6).
   - `clusterAccess`: whether this repo's sessions get the `kubectl` shim
     that RPCs to thot-executor (`docs/adr/0037`). A privilege grant, so it
     exists only here, behind a reviewed infra-bootstrap PR. Currently true
     for `infra-bootstrap` alone.

   An entry whose `id` does not match is ignored with an ERROR. An empty file
   revokes every override; a missing one changes nothing.
3. **Removal**: take the repo out of the App's installation (or delete it).
   The repo is hidden on the next tick and its row is deleted 7 days later,
   once no open session references it. More than 2 repos vanishing in one
   tick changes nothing and logs an ERROR (shrink guard): check the
   provisioner's `github app:` log lines first. To keep the fleet off a repo
   without removing it, switch it off in Settings → Repos instead.

There is nothing else to configure. No profile, no start command, no e2e
preview, no `run_command` — `docs/adr/0048` §6 deleted all of it along with
the sandbox pod.

### Bumping the browser toolchain

The agent's browser tools come from `@playwright/mcp` running as a **stdio
MCP server inside the worker container itself** (`worker/src/session.ts`'s
`playwrightMcpServer()`), so there is no snapshot to refresh and no tool list
to fetch — the SDK discovers them over the pipe it owns. The committed
playwright tool snapshot the sidecar used to serve, and the `e2e-runner`
image behind it, both died with `docs/adr/0048` §6.

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

A newly installed repo is live on the next tick, selectable in the
new-session dialog fleet-wide. The only infra-bootstrap edit is an override
(step 2), never a per-repo Application.

## 2. Release / deploy walkthrough

`.github/workflows/docker.yml` is the pipeline; read it for the detail.
One workflow, three jobs on a push to `main`:

```
push to main
  → release (ubuntu-latest; gated on worker-test and dashboard-test):
      release-it computes the version from Conventional Commits, bumps
      package.json + CHANGELOG.md, pushes the version commit and tag
  → build-push (ONE job, on [self-hosted, ukubi-build] — the build-runner LXC):
      loops $COMPONENTS (core provisioner sidecar executor worker migration),
      `buildah` builds, pushes content-addressed `src-<hash>` tags to
      registry.bnei.lan:5000 (ukubi's own Zot, infra-bootstrap ADR-0034).
      An image whose inputs did not change is not rebuilt. On a PR only the
      changed components build, and nothing is pushed.
  → deploy (push only): pins each image's tag in k8s/core.yaml (core,
      migration) and k8s/provisioner/*.yaml (provisioner, worker, sidecar),
      writes k8s/images.env and FLEET_VERSION, checks every pin landed,
      commits to main with the built-in token (no workflow re-trigger)
  → ArgoCD: core (two-source Application, chart from infra-bootstrap +
      k8s/core.yaml here) and provisioner (standalone plain-manifest
      Application, k8s/provisioner/ here) sync the new pins
```

`workflow_dispatch` builds what the registry lacks on `main` and pushes
nothing: it measures the build loop without cutting a release.

Check current live tags: `grep 'tag:' k8s/core.yaml; grep 'image:'
k8s/provisioner/deployment.yaml`. Check what actually reached the registry —
a green build is not the same claim, and anonymous read means no auth:

```bash
curl -s http://registry.bnei.lan:5000/v2/_catalog
curl -s http://registry.bnei.lan:5000/v2/agent-fleet-worker/tags/list
```

Retention keeps only the **last 3 tags per image** plus `latest`
(infra-bootstrap `gitops/platform/values/zot/values.yaml`), so a rollback
deeper than 3 releases needs the images rebuilt and pushed, which no
workflow path does today (`workflow_dispatch` builds `main` and pushes
nothing). Runner/build/disk problems on the box itself go
through infra-bootstrap's `/build-runner-ops`.

Check what's actually running:
`kubectl get pods -n agent-fleet -o wide` — expect `core` and
`provisioner` Deployments, zero-or-more two-container
`worker-<shortSessionId>` Pods (one per warm session — see
`WorkerResourceName`/`shortID` in `provisioner/internal/k8s/names.go`), and
a completed `agent-fleet-browser-cache` Job (via `/k8s-ops` in
`infra-bootstrap` if you need cluster access set up).

Never merge to `main` or edit `k8s/core.yaml`/`k8s/provisioner/*.yaml`'s
pinned versions without confirming — every push to `main` cuts a release, and
a release redeploys the two persistent
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

-- the live count MAX_IN_FLIGHT_TASKS (default 5) is checked against. Keep this list in
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

-- what a session actually did: its transcript
SELECT seq, "from", type, left(text, 200), created_at
FROM transcript WHERE session_id = '<sessionId>' ORDER BY seq;

-- the repos the fleet can reach, and which it uses (docs/adr/0074)
SELECT name, url, base_branch, image, cluster_access, active FROM repos ORDER BY name;
```

Connect via the `AGENTFLEET_DB_*` credentials (Infisical, `agent-fleet-nygh`
project) — `core` is the **only** component with these
(`docs/adr/0020` point 1); `provisioner`/`sidecar`/`worker` hold none.
Pod lifecycle events update `pod_phase` but are not journaled
(`docs/adr/0055`); their history is in Loki. For diagnosing *why* a specific
session is stuck rather than just listing state, use `/fleet-debug` instead.
