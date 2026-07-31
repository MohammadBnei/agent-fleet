---
name: fleet-ops
description: Onboard a new target repo/worker, walk the release/deploy pipeline, or inspect the live task queue for agent-fleet. Use when the user wants to add a new repo to the fleet, deploy a release, or check what tasks are running/stuck.
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
this assumes.

## 1. Onboard a new target repo/worker

A new worker is a values file here + one line in `bot/`'s allowlist + one
registration in the *other* repo (`infra-bootstrap`). All four steps:

1. **`k8s/<repo>-worker.yaml`** — copy `k8s/dream-analyst-worker.yaml`,
   change `env: TARGET_REPO`/`TARGET_REPO_URL`/`WORKER_NAME`. Set
   `BASE_BRANCH` too if the target repo doesn't develop off `main` (see
   `vos-monolith-worker.yaml` for the pattern — its default branch is `dev`).
   Keep `image.tag` matching whatever's already pinned in the other
   `k8s/*.yaml` files — the deploy job bumps all of them together.
2. **`bot/src/db.ts`** — add the repo name to `KNOWN_REPOS`. Without this,
   `/task`'s dropdown won't offer it and the legacy `!task` regex won't
   match it either (`TRIGGER_RE` is built from `KNOWN_REPOS`).
3. **`infra-bootstrap`'s `gitops/apps/registry.yaml`** — register the new
   Application as a two-source entry (chart from `infra-bootstrap`, values
   from `agent-fleet`'s `k8s/<repo>-worker.yaml`), same shape as the
   existing `dream-analyst-worker`/`vos-monolith-worker` entries. Use that
   repo's own `/add-app` skill for this — don't hand-edit the
   ApplicationSet.
4. **Infisical** — confirm the target repo's bot GitHub account (`GH_TOKEN`)
   has collaborator access to open PRs there. The rest of the secrets
   (`CLAUDE_CODE_OAUTH_TOKEN`, `AGENTFLEET_DB_*`, `REDIS_*`) are already
   shared across the `agent-fleet-nygh` Infisical project — no new secret
   needed unless the new repo needs something worker-specific.

This touches two repos and shared cluster state — confirm with Mohammad
before pushing the `infra-bootstrap` registry edit or merging here.

## 2. Release / deploy walkthrough

```
git push origin <tag>              # any tag push triggers docker.yml
  → build-push (matrix: worker, bot): build, push to Docker Hub, Trivy scan
  → deploy (on push only): sed-bumps k8s/*.yaml's `tag: "..."`, commits to main
      (uses the default GITHUB_TOKEN — deliberately doesn't re-trigger release.yml)
  → ArgoCD (two-source Application, chart from infra-bootstrap + these values)
      picks up the new tag and syncs
```

`release.yml` is separate and unrelated to the above — it runs
`release-it` on ordinary pushes to `main` (paths-ignoring `**.md`,
`k8s/*`, `package.json`) to bump the version/CHANGELOG from Conventional
Commits. It does not build or deploy anything.

Check current live tag: `grep 'tag:' k8s/*.yaml`. Check what's actually
running: `kubectl get pods -n agent-fleet -o wide` (via `/k8s-ops` in
`infra-bootstrap` if you need cluster access set up).

Never push a tag or edit `k8s/*.yaml`'s pinned version without confirming
— this redeploys three live apps.

## 3. Inspect the live task queue

```sql
-- what's running/stuck right now
SELECT id, repo, status, claimed_by, created_at, updated_at
FROM tasks
WHERE status NOT IN ('done', 'cancelled')
ORDER BY created_at;

-- recent history for one repo
SELECT id, status, pr_url, created_at, updated_at
FROM tasks WHERE repo = '<repo>' ORDER BY created_at DESC LIMIT 20;

-- what a task's worker actually did
SELECT event_type, payload, created_at
FROM knowledge_journal
WHERE payload->>'taskId' = '<taskId>'
ORDER BY created_at;
```

Connect via the `AGENTFLEET_DB_*` credentials (Infisical, `agent-fleet-nygh`
project) — same database both `bot/` and every `worker/` use
(`agentfleetdb`, Pigsty-managed). For diagnosing *why* a specific task is
stuck rather than just listing state, use `/fleet-debug` instead.
