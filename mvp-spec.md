# Spec: Agent Fleet MVP — Golden Path

**Status:** Draft v1 (spec phase — no code yet)
**Supersedes for active work:** [`design-v0.md`](./design-v0.md) (full v2 vision, parked)
**Cluster:** `ukubi-cluster` (self-hosted Kubernetes, see `infra-bootstrap`)

---

## Objective

Prove the fleet's core loop works, end to end, before building any of the
surrounding platform:

> A message in Discord triggers exactly one isolated Claude Code worker,
> which plans, codes, tests, and documents a feature against a disposable
> sandbox repo, opens a PR, and replies in the Discord thread with a summary
> and a link.

This is `design-v0.md`'s §10 Phase 0+1, deliberately trimmed further:

- **No staging auto-deploy** (`design-v0.md` §10 Phase 2) — the MVP ends at
  an open PR. A human merges by hand.
- **No bug-handling loop** (`design-v0.md` §4, §10 Phase 4) — out of scope
  until the golden path has run for real.
- **No orchestration layer** (`design-v0.md` §11.1) — Hermes and OpenClaw are
  both excluded. The doc itself notes "Claude Code's own AgentTool primitives
  may be sufficient... without Hermes or OpenClaw at all" — this MVP is the
  test of that claim.
- **No platform services beyond secrets** — LiteLLM, Redis, the Postgres
  ledger, MinIO, and Prometheus/Grafana/Loki (`design-v0.md` §6, §10 Phase 0)
  are all deferred. Visibility is `kubectl logs` plus the Discord thread
  itself — not a dashboard.
- **Target repo is a disposable sandbox**, not `voc`/`dreamer` — plumbing
  bugs (worktree isolation, PVC lifecycle, PR flow) get caught somewhere
  low-stakes first.

**User:** Mohammad, solo developer, sole operator and sole reviewer of every
PR this produces.

**Why this scope:** the full design doc bundles an unresolved orchestrator
choice, 7 platform services, and a staging/production gate into "v1." None
of that can be validated until one task has gone from Discord message to
merged-ready PR a single time. This spec exists to force that one proof
point before any further investment.

---

## Tech Stack

| Layer | Choice | Notes |
|---|---|---|
| Worker runtime | Claude Code, headless (`claude -p` / SDK), one Kubernetes Job per task | Own PVC + git worktree/branch per task. No shared writable repo PVC. |
| Orchestration | None — a thin trigger spawns the Job directly | Hermes/OpenClaw excluded for MVP; revisit only when N agents must run concurrently (`design-v0.md` §11.1). |
| Discord ingress + callback | Minimal custom bot (not OpenClaw) | Watches a channel/thread for trigger messages, calls the k8s API to create the Job, and posts the reply when the worker calls back. |
| Secrets | External Secrets Operator, existing SOPS/age backend (already used by `infra-bootstrap`'s `gitops/`) | Three secrets only: Discord bot token, Anthropic API key, git host token (PAT) for the sandbox repo. |
| Output | Git branch + PR on the sandbox repo | No MinIO, no artifact store — the PR itself is the artifact. |
| Visibility | `kubectl logs` + Discord thread | No Postgres ledger, no Prometheus/Grafana/Loki. |

**Explicitly not used in this spec:** Hermes, OpenClaw, LiteLLM/OpenRouter
gateway, Redis pub/sub, Postgres task ledger, MinIO, Prometheus, Grafana,
Loki, ClickHouse. Each may return once the golden path has run and the next
`design-v0.md` phase is greenlit.

---

## Commands

No build/test commands exist yet — this is the spec phase. Once
implementation starts, this section should be filled in with the actual
commands for whatever the Discord bot ends up being built in (likely a small
Node or Python service), e.g.:

```
Build (bot):   <tbd — depends on chosen runtime>
Run (bot, local): <tbd>
Deploy (bot):  kubectl apply -f manifests/ (or via gitops/, see below)
Worker image:  docker build -t agent-fleet-worker .
Logs:          kubectl logs -n agent-fleet job/<task-id>
```

---

## Project Structure

This repo (`agent-fleet`, submoduled into `infra-bootstrap` at
`agent-fleet/`) is where the MVP's actual source will live once
implementation starts:

```
agent-fleet/
  design-v0.md       → parked full-vision doc (context, not active spec)
  mvp-spec.md         → this file
  bot/                 → Discord bot source (TBD — not built yet)
  worker/              → Claude Code worker Job template/Dockerfile (TBD)
  manifests/           → k8s manifests for the above (TBD)
```

Deployment onto `ukubi-cluster` follows `infra-bootstrap`'s existing GitOps
pattern: once `manifests/` exists, it gets registered as a new app via
`infra-bootstrap`'s `/add-app` skill (registry.yaml + the ApplicationSet),
reusing `gitops/platform/common-app-chart` — never a bespoke per-app chart.

---

## Code Style

No code exists yet. When it does:
- Anything that becomes a Kubernetes manifest follows
  `infra-bootstrap/gitops/platform/common-app-chart` conventions — don't
  invent a parallel chart.
- The Discord bot should stay small enough not to need its own framework
  beyond a Discord client library and a Kubernetes client library.

---

## Testing Strategy

The "test" for this MVP *is* the golden path itself — there's no separate
test suite for the fleet plumbing at this size:

1. Post a trigger message in the designated Discord channel/thread.
2. Confirm exactly one Kubernetes Job spawns (`kubectl get jobs -n agent-fleet`).
3. Confirm the worker gets an isolated PVC + git worktree/branch — not a
   shared repo checkout.
4. Confirm the worker's own plan → code → tests → docs cycle runs, and the
   sandbox repo's own test suite passes inside the Job before it opens a PR.
5. Confirm a PR opens on the sandbox repo with the resulting code, tests, and
   docs.
6. Confirm the Discord thread receives a reply with a summary and the PR
   link, within the task timeout.

If the sandbox repo has no test suite yet, one must be added first — a
worker that "passes tests" against an empty test suite proves nothing.

---

## Boundaries

- **Always:**
  - Worker tools limited to `bash`, `read`, `write`, `edit`. Deny `browser`
    and `deploy` (per `design-v0.md`'s task envelope, §8).
  - Every result is a PR. No auto-merge, ever.
  - Secrets only via External Secrets — never committed, never passed as
    plain env vars in a manifest.
  - One PVC + one git worktree per task — never a shared writable repo PVC
    across workers.

- **Ask first:**
  - Pointing the worker at real `voc` or `dreamer` repos (MVP targets a
    disposable sandbox only).
  - Adding back any deferred platform service (LiteLLM, Redis, Postgres
    ledger, MinIO, Prometheus/Grafana/Loki) before the golden path has
    actually run.
  - Reintroducing Hermes or OpenClaw before the single-worker MVP is proven.

- **Never:**
  - Fleet-managed `infra-bootstrap` cluster ops — that repo's own `CLAUDE.md`
    states a human runs kubespray/ansible/pigsty personally, and this is
    explicitly blocked until that decision is revisited (`design-v0.md`
    §11.4).
  - Autonomous production deploy — not even for the MVP's own bot/worker
    manifests.
  - Committing Discord/Anthropic/git tokens to git, in this repo or any
    other.

---

## Success Criteria

- [ ] A trigger message in the designated Discord channel/thread results in
      exactly one Kubernetes Job being created — never zero, never more than
      one per message.
- [ ] The worker Job runs in an isolated workspace (its own PVC and git
      worktree/branch) against the disposable sandbox repo.
- [ ] The worker completes plan → code → tests → docs and opens a PR against
      the sandbox repo, with the sandbox repo's own tests passing inside the
      Job.
- [ ] The originating Discord thread receives a reply containing a summary
      and the PR link, within the task timeout (1800s default, per
      `design-v0.md` §8).
- [ ] None of the deferred components (Hermes, OpenClaw, LiteLLM, Redis,
      Postgres ledger, MinIO, Prometheus/Grafana/Loki, staging deploy, bug
      loop) are required for any of the above to be true.

---

## Open Questions

1. **Discord bot hosting shape** — in-cluster Deployment on `ukubi-cluster`
   (proposed default, since ArgoCD/Traefik/MetalLB are already solved per
   `design-v0.md` §3) vs. something simpler run outside the cluster.
2. **Git host / token scope for PR creation** — a personal access token
   scoped to the sandbox repo only (proposed default) vs. a GitHub App.
3. **Callback wiring** — the worker Job calls the Discord webhook directly on
   completion (proposed default, no extra moving part) vs. a small poller
   service watching Job status.
4. **Sandbox repo** — create a fresh disposable repo for this, or reuse an
   existing throwaway one. Not yet decided.

---

## Sources

- [`design-v0.md`](./design-v0.md) — full v2 design doc this spec trims down
  from. Refer to it for the parked scope (designer/community-manager roles,
  staging/production gate, bug-handling loop, cost caps, orchestration
  internals debate).
