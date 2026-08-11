# ADR-0012: On-demand e2e test environments via a standalone e2e-provisioner

**Status:** Accepted — except its **path-based routing** decision, superseded by
[ADR-0038](0038-per-task-subdomain-e2e-preview.md). Previews are now one
subdomain per task with the app at `/`; the "no wildcard DNS exists anywhere in
this cluster" premise below stopped being true when `bnei.dev` moved to
Cloudflare (`infra-bootstrap` ADR-0033). Everything else here stands.
**Date:** 2026-08-01

## Context

Today, trust in a task's correctness comes only from reading the diff
before merge — there's no way for the worker's Claude session, or Mohammad,
to actually run a task's branch and watch it behave before a PR is
approved. For tasks that need real e2e/browser verification (Playwright-
driven UI flows, or curling a live backend), a diff alone isn't enough.

This was designed through an architecture interview and an adversarial
(doubt-driven) review before any code was written — several early ideas
were rejected during that process for concrete reasons, kept below as
Alternatives rather than re-derived from scratch here.

## Decision

During the implementation phase only (write/edit already unlocked per
ADR-0005), the worker's Agent SDK session can call `request_e2e_env` to get
an ephemeral pod running code-server (human review only), the task's app
built from its branch, and a Playwright MCP server — plus a live LAN
preview URL for Mohammad. It tears down when the task's status reaches
`done`/`failed`/`cancelled`, or on an explicit kill from either the human
(`/e2e-kill` in Discord) or the agent itself (`kill_env`) — never merely
because a PR was opened, since review/iteration can continue after that.

**The worker pod gets zero new Kubernetes RBAC, ever.** A new, separate
service — `e2e-provisioner` — holds the only RBAC in the entire fleet that
can create/delete Pods/Services/IngressRoutes/Middlewares (namespaced
`Role`, never a `ClusterRole`). The worker only ever talks to it as an MCP
client over the network (`http` transport, not `stdio` — the provisioner is
a persistent service, not a per-session subprocess like `mcp-redis`).

No `exec`/`cp` grant on the worker either. The agent drives Playwright
testing via MCP tool calls proxied through the provisioner (results come
back inline in the tool response — nothing to pull off disk after the
fact), and curls the app's own port directly for backend/API testing. A
`NetworkPolicy` allows the worker pod to reach the e2e pod's app port but
denies its code-server port — code-server is a full IDE/terminal, a
human-review-only surface that would otherwise let the agent bypass the
whole git-worktree → commit → PR flow.

`common-app-chart` has no ServiceAccount/RBAC/NetworkPolicy support at all,
so `e2e-provisioner` is deployed as a standalone plain-manifest ArgoCD
Application (`infra-bootstrap/gitops/platform/e2e-provisioner/`), modeled
exactly on `gitops/platform/actions-runner/` — infra-bootstrap already
established this as the correct escape hatch for "needs its own RBAC, isn't
a typical web app," not a new pattern invented here. This does **not**
violate this repo's own forbidden-pattern list (`docs/DECISIONS.md` §2,
"a bespoke per-app Helm chart") — that bans writing a new Helm chart, not a
plain-manifest Application; `actions-runner` already carved out that
distinction.

GPU acceleration is explicitly out of scope for this decision — the NVIDIA
device plugin and a GPU node taint don't exist anywhere in the cluster yet,
which would be its own infra buildout. v1 runs CPU-only headless Chromium;
the e2e pod has a *preferred* (not required) nodeAffinity toward the GPU
node so it can pick this up later without a design change.

Path-based routing under one static host (`e2e.bnei.dev/<taskId>/app/`,
`/<taskId>/code/`), not per-task subdomains — no wildcard DNS exists
anywhere in this cluster (ADR-0001 already rejected DNS-01/wildcard-cert
issuance), so per-task subdomains aren't available without new ACME
issuance per session. Preview auth reuses the existing `basic-admin-auth`
Middleware (already gating pgweb/Alertmanager) rather than minting a
per-session Secret — no new credential to manage per e2e run.

The worker's own per-repo workspace PVC moves from `ReadWriteOnce` to
`ReadWriteMany` so the e2e pod can mount the same task worktree (via a
per-task `subPath`, never the PVC root — one task's e2e pod can never see
another concurrent task's checkout).

## Alternatives considered

- **Direct k8s RBAC on the worker pod itself** (agent runs kubectl/helm
  directly) — rejected: too large a blast-radius jump for a long-lived
  autonomous agent; the whole point of this design is that the thing
  holding write-in-git trust (ADR-0005) never also holds infra-mutation
  trust.
- **Route e2e provisioning through the existing self-hosted GitHub Actions
  runner** (ADR-0022/0023 in infra-bootstrap) — rejected: that runner's
  model fits finite CI/CD jobs, not an open-ended "stay alive until a human
  or the agent says stop" lifecycle.
- **Ephemeral per-task Kubernetes Job spun directly by the worker** —
  rejected: same RBAC-expansion problem as above, and reintroduces the
  per-task-pod-lifecycle pattern ADR-0003 already moved away from for the
  worker itself.
- **Worker-pod `exec`/`cp` RBAC to pull Playwright artifacts off the e2e
  pod's disk** — rejected after adversarial review: Kubernetes RBAC has no
  concept of "only this task's own pod," so any such grant is necessarily
  fleet-wide (task A could exec into task B's pod), and `exec` is a live
  shell regardless of intent, not a read-only primitive. A Playwright MCP
  server proxied through the provisioner replaces this entirely — results
  come back as tool-call responses, no file pull-back needed.
- **Playwright MCP endpoint reachable directly by the worker** — rejected:
  the e2e pod's network address isn't known until after `request_e2e_env`
  returns, and the Agent SDK sets a session's `mcpServers` once per
  `query()` call (no supported way to add a server mid-session on the
  stable API). The provisioner instead exposes one stable
  `/mcp/:taskId` endpoint for the life of the session and proxies to
  whichever e2e pod is currently live for that task.
- **Per-session Secret + Middleware for preview auth** — rejected in favor
  of reusing the existing `basic-admin-auth` Middleware: no new credential
  to generate/rotate/store per e2e run.
- **Wildcard DNS / per-task subdomains** — rejected: no wildcard DNS exists
  in this cluster and ADR-0001 already ruled out the ACME approach that
  would require. One static host + path-based routing sidesteps per-session
  cert issuance entirely.
- **Standing up GPU scheduling (NVIDIA device plugin + node taint) as part
  of this decision** — deferred: real infra buildout unrelated to this
  feature's critical path, decoupled as a fast-follow.

## Consequences

- `e2e-provisioner` is a new, narrowly-scoped RBAC surface — the only one in
  the fleet — and a new coordination point (`e2e_sessions` table) alongside
  `tasks`/`knowledge_journal`.
- The per-repo workspace PVC's access-mode migration (RWO→RWX) is a real
  `kubectl delete pvc` + ArgoCD-recreate, not a quiet reconcile — done in
  its own deploy window, not bundled with this feature's code.
- This is the first `NetworkPolicy` anywhere in the cluster. Cilium already
  enforces it with no additional setup, but it's worth knowing it exists as
  a precedent for future network-segmentation needs.
- Cross-repo image-tag drift: `e2e-provisioner`/`e2e-runner` images are
  built by agent-fleet's CI but deployed from infra-bootstrap manifests
  outside that CI's auto-bump — accepted for v1 (floating tag), same as
  `actions-runner`'s own precedent.
- No auth on the provisioner's own MCP endpoint beyond cluster-network
  reachability — acceptable for a single-tenant homelab (same bar as
  `mcp-redis`), with a shared-secret-header upgrade path if the threat
  model ever changes.
