# AI Agent Fleet — Infrastructure Design Document

**Status:** Draft v2 (v1 scope confirmed via interview; orchestration internals still open)
**Date:** 2026-07-20
**Author:** Mohammad-Amine Banaei
**Cluster:** Self-hosted Kubernetes (Kubespray) on Proxmox VMs (`ukubi-cluster`)

---

## 1. Confirmed Intent (v1)

- **Outcome:** A self-hosted agentic dev fleet where multiple Claude Code
  workers — each owning one feature end-to-end (plan → code → tests → docs)
  — run in parallel against real products, coordinated by shared infra
  (PVC sharing, pub/sub, observability), auto-deploying to staging with a
  human gate before production, plus a reactive loop that jumps on
  production bugs.
- **User:** Mohammad, solo developer, acting as manager of the fleet. Not a
  team, no external users of the fleet itself.
- **Why now:** Already running Claude Code heavily and hitting the ceiling
  of doing that by hand. Coordinating several instances without a
  context-switching nightmare needs real infrastructure — and this repo
  (built in two weekends) already proves that infra can be stood up fast.
- **Success:** Real, shippable progress on **voc** and **dreamer** lands
  faster than solo work — not an "org" that exists but ships nothing real.
  Full visibility at all times: what's running, where, and why.
- **Constraint:** No hard cost cap yet — visibility first. Deploy autonomy
  stops at staging; production always needs human sign-off. No per-aspect
  agent splitting on a single feature — one agent owns a feature, fully.
- **Out of scope (v1):** designer/community-manager roles; a decided
  orchestration stack; the fleet operating this cluster's own
  kubespray/ansible/pigsty for real; a fixed monthly spend ceiling. See
  §11 Open Decisions.

Core principles carried from v1 research, still valid:

- **GitOps is the single source of truth.** No agent UI owns state; every
  workload, secret reference, model route, and policy is declared in git
  and reconciled by Argo CD.
- **Discord is the human surface.** Approvals (staging→prod), bug alerts,
  and task visibility flow through Discord. Ops panes (Argo CD, Grafana)
  stay separate.
- **Maturity-aware.** Fast-moving 2026 projects; only first-party sources
  are treated as authoritative. Versions pinned, git repo is the recovery
  point.

---

## 2. Scope: v1 vs Later

| | v1 (this doc's target) | Later (parked, not designed yet) |
|---|---|---|
| Roles | Dev only — one agent owns a feature fully | Designer, community manager |
| Targets | `voc`, `dreamer` (real alpha apps) | This repo (`infra-bootstrap`) — **blocked**, see §11.4 |
| Deploy | Auto to staging; human-gated to production | Autonomous production deploy |
| Public action | None — agents don't act on real users/channels | Autonomous Discord/Twitter posting, design publishing |
| Cost | Visibility only (ledger/dashboard) | Hard spend caps once a usage baseline exists |
| Coordination granularity | One agent = one feature, end-to-end | N/A — this is a locked v1 decision, not a phase |

The failure mode being explicitly avoided: splitting a single feature across
specialist agents (a frontend agent + backend agent + docs agent all
touching the same feature concurrently). That produces coordination
overhead, not speed. The fleet scales **horizontally** — more features in
flight at once — not by decomposing one feature vertically.

---

## 3. Existing Infrastructure

| Component | Role |
|---|---|
| **3× Proxmox VMs** | Kubespray-provisioned Kubernetes nodes |
| **MetalLB** | VIP / load balancing (L2/L3) |
| **Traefik** | Ingress controller |
| **HA PVC backends** | In progress — needed for stateful agent workspaces (still pending, see §11.5) |

**Implication:** Argo CD, ingress, and LB are already solved problems. The
remaining work is the agent-platform layer (model gateway, coordination,
artifact store, observability, bug-handling loop) plus containerising
runtimes that ship no official images.

---

## 4. Task/Agent Model (v1 core decision)

- **Unit of work = one feature.** A single Claude Code worker owns it
  end-to-end: plan, implement, write tests, write docs.
- **Fleet = N feature-owning workers in parallel**, each in its own git
  worktree/PVC, targeting `voc` or `dreamer`. No shared writable repo PVC
  across agents.
- **No auto-merge.** Every feature lands as a PR.
- **Deploy path:** tests pass → auto-deploy to staging → Discord
  notification with diff/summary → human approves → production.
- **Bug-handling loop (new in v2):** production logs (Loki) feed a log
  analytics layer (ClickHouse) → anomaly/error triggers an alert → alert
  spins up a Claude Code worker scoped to the failing service → fix follows
  the same PR → staging → human-approval → production path as any other
  feature. No direct-to-prod bypass for bug fixes either.

---

## 5. Runtime Inventory (verified, first-party — candidates, not yet chosen)

Orchestration internals are an **open decision** (§11.1). The following
runtimes were verified against their primary GitHub repos as candidates;
none are committed to yet.

### 5.1 OpenClaw
- **Repo:** [github.com/openclaw/openclaw](https://github.com/openclaw/openclaw)
- Personal AI assistant / multi-channel gateway (pnpm monorepo, Node 24).
  Always-on control plane managing agents, channels, tools, events.
- **Deployment model:** launchd/systemd user service via
  `openclaw onboard --install-daemon`. No official container image, no
  Kubernetes mention.
- **Sandbox backends:** Docker (default), SSH, OpenShell. Per-session
  allow/deny of tools (`bash`, `process`, `read`, `write`, `browser`,
  `canvas`, `cron`, etc.).
- **Candidate role:** Discord edge ingress + channel identity + sandbox
  policy — *if* chosen over a thinner Discord bot.

### 5.2 Hermes Agent
- **Repo:** [github.com/NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
- Orchestrator-worker framework by Nous Research. Non-blocking delegation
  (`delegate_task(background=true)`), persistent memory, Kanban, MCP tools.
- **Deployment model:** curl install script, Linux/macOS/WSL. No official
  image, no native k8s.
- **Key capability:** can spawn Claude Code for heavy coding tasks and fold
  results back into memory.
- **Candidate role:** top-level coordinator — reads/writes task state,
  delegates whole features to Claude Code workers, does **not** decompose
  a feature into sub-tasks (per §4).

### 5.3 Claude Code
- **Vendor:** Anthropic (`@anthropic-ai/claude-code`).
- **Headless mode:** `claude -p` / SDK — process, no TTY, Job-style
  execution.
- **Coordination:** AgentTool abstraction (sync, async, fork, teammate,
  remote/CCR). Git worktree isolation per spawned agent. Experimental agent
  teams via `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`.
- **Fleet role (settled):** the feature-owning worker itself — short-lived
  Kubernetes Job/Sandbox doing the actual plan/code/test/docs work in an
  isolated worktree.
- **Note:** Claude Code's own AgentTool primitives (fork/teammate) may be
  sufficient for coordination without Hermes or OpenClaw at all — this is
  part of the open orchestration debate, not a rejected option.

### 5.4 AgentTier (k8s-native sandbox — building block)
- **Repo:** [github.com/AgentTier](https://github.com/bradagi/awesome-cli-coding-agents) (Apache-2.0 k8s-native sandbox runtime)
- `Sandbox` CRD: Pod + PVC + NetworkPolicy (optional gVisor), streams
  stdout/stderr/exit as SSE. Ships reference templates for Claude Code.
- **Fleet role:** candidate k8s-native execution substrate for workers —
  Pod isolation, PVC per task, egress policy without hand-rolled Jobs.

> **Maturity note:** only first-party repos above are authoritative.
> Community/second-party projects (HermesHQ, RCC, Hermes Workspace/Bible/
> Atlas, Ruflo/Claude Flow, CMux) are optional operator surfaces for later
> evaluation, never source of truth.

---

## 6. Platform Building Blocks (confirmed)

| Component | Choice | Purpose |
|---|---|---|
| Model gateway | **LiteLLM Proxy** (or OpenRouter) | Single place for provider keys, per-agent usage tracking. No hard budget cap in v1 — visibility only. |
| Inter-agent comms | **Redis pub/sub** | Coordination signal between fleet components (not per-feature sub-task chatter — see §4). |
| Shared state / sharing | **PVC sharing between agents** | Easy artifact/workspace sharing where needed, without giving agents a shared *writable* repo PVC. |
| Task ledger | **Postgres** | Status, results, cost per task — the "what's running, where, why" record. |
| Artifact store | git branches/PRs + **MinIO** | Code changes via PRs; transcripts/artifacts in MinIO. |
| Log analytics (bug loop) | **Loki → ClickHouse** | Loki collects logs; ClickHouse holds the analytics layer that detects the anomalies which trigger the bug-handling loop (§4). |
| Observability | Prometheus + Grafana + Loki | Metrics + logs, labeled `task_id`, `agent_id`, `runtime`, `model`, `cost`. This *is* the "controlled agents: know what runs, where, why" requirement — not optional polish. |

Cost stance: **visibility before caps.** Build the ledger/dashboard first;
set `budget.max_usd` enforcement (§8) only once a real usage baseline
exists (§11.3).

---

## 7. Discord as the Human Surface

Discord's hierarchy maps onto the fleet structure and doubles as the
approval mechanism for staging→production and the notification channel
for the bug-handling loop.

### Topology
| Discord object | Fleet meaning |
|---|---|
| Server | The whole fleet |
| Category | Project (`voc`, `dreamer`) — mirrors k8s namespaces |
| Channel | A long-running coordinator or a per-project stream |
| Thread | A task (one feature, or one bug fix) — `task_id` = thread id |
| Forum channel | Queued work — each post is a task awaiting a worker |

### Flow
```
Discord message ──▶ ingress ──▶ task envelope ──▶ coordinator ──▶ Claude Code worker
                                                          │
result + summary ──▶ coordinator ──▶ Discord thread reply (with MinIO link)
                                                          │
                                          staging deploy ──▶ approval reaction (✅/❌)
                                                          │
                                                    production deploy
```

Bug loop:
```
Loki → ClickHouse (anomaly detected) ──▶ alert ──▶ Discord thread opened
                                              │
                                    Claude Code worker spawned (scoped to failing service)
                                              │
                                    fix PR ──▶ staging ──▶ Discord approval ──▶ production
```

### Wiring details
- **Identity & permissions:** Discord role-based allowlists; only admin
  role may approve production promotion.
- **Sandboxing:** untrusted/group sessions sandboxed; not relevant yet
  since v1 has no external-facing roles.
- **Long messages:** chunk to Discord's 2000-char limit; full transcripts →
  MinIO, reply with link + summary.
- **Approvals:** staging→production promotion is the one hard gate in v1
  — surfaced as a Discord thread reply (✅/❌).
- **Secrets:** Discord bot token in External Secrets (SOPS/age), never in
  git.

---

## 8. Task Envelope (communication contract)

```json
{
  "task_id": "uuid",
  "thread_id": "discord thread id (human surface)",
  "agent_id": "cc-worker-3",
  "runtime": "claude-code",
  "repo": "git@…/voc | git@…/dreamer",
  "branch": "feat/task-uuid",
  "goal": "Implement multi-tenant billing export",
  "allowed_tools": ["bash", "read", "write", "edit"],
  "denied_tools": ["browser", "deploy"],
  "budget": { "max_tokens": 500000, "max_usd": null },
  "timeout": 1800,
  "artifact_paths": ["s3://minio/tasks/uuid/report.md", "s3://minio/tasks/uuid/diff.patch"],
  "callback": { "channel": "discord", "thread_id": "…", "on_result": "reply" },
  "approval_required": "staging_to_prod_only"
}
```

`budget.max_usd: null` is deliberate for v1 — tracked and visible via the
ledger, not enforced, until a usage baseline exists (§11.3).
`approval_required` is scoped to production promotion only; staging is
automatic once tests pass.

Outputs land in predictable locations: git branch/PR, MinIO artifact, or
Postgres task result.

---

## 9. Secrets & Credentials

- **External Secrets Operator** with SOPS/age (or Vault/1Password/Bitwarden
  backend).
- **Never commit** API keys or OAuth refresh tokens to git.
- **Model credentials route through LiteLLM/OpenRouter** — agents call the
  gateway with a scoped key, never hold broad provider secrets.
- **Per-agent service accounts** with least-privilege Kubernetes RBAC.
- Discord bot token, Anthropic/OpenAI keys, OpenRouter key, git deploy keys
  → all in ExternalSecrets.

---

## 10. Phased Rollout (v1)

| Phase | Deliverable | Status |
|---|---|---|
| **0 — Platform base** | Argo CD app-of-apps, External Secrets + SOPS/age, LiteLLM gateway (no cap), Redis pub/sub, Postgres ledger, MinIO, Prometheus/Grafana/Loki. | Pending |
| **1 — Golden path + Discord loop** | One queued feature → one Claude Code worker (own worktree/PVC) → PR with code+tests+docs. Discord thread per task from day one — visibility, not deferred. | Pending |
| **2 — Staging/production gate** | Tests-pass → auto-deploy staging → Discord approval reaction → production. No auto-merge, no auto-prod. | Pending |
| **3 — Coordination layer** | Resolve §11.1 (Hermes vs OpenClaw vs plain Claude Code AgentTool) and run N features in parallel for real. | Pending |
| **4 — Bug-handling loop** | Loki → ClickHouse → alert → scoped Claude Code worker → same PR/staging/approval path. | Pending |
| **5 — Hardening** | Per-project namespaces + NetworkPolicies, cost caps once baseline usage known, KEDA autoscaling. | Pending |
| **6 — Parked** | Designer/community-manager roles; autonomous public actions; fleet-managed `infra-bootstrap` (blocked, §11.4). | Future, undesigned |

---

## 11. Open Decisions / Next Steps

1. **Orchestration internals** — Hermes vs OpenClaw vs plain Claude Code
   AgentTool (fork/teammate/remote) for the coordination layer (§10 Phase
   3). Genuinely undecided; needs its own design pass, not a default.
2. **Which app first** — `voc`, `dreamer`, or both concurrently. Not yet
   decided.
3. **Cost cap** — none in v1. Revisit `budget.max_usd` enforcement once a
   real usage baseline exists from Phase 0/1.
4. **Fleet-managed `infra-bootstrap`** — explicitly wanted eventually, but
   **blocked today**: this repo's own `CLAUDE.md` states a human runs
   kubespray/ansible/pigsty personally, and this session is not the
   autonomous agent. Enabling the fleet to operate this cluster for real
   requires an explicit update to that decision first — not a silent
   override.
5. **HA PVC backend** — must be resolved before stateful workspaces
   (carried over from v1, still pending).
6. **Designer/community-manager autonomy** — parked. Whether those roles
   ever act autonomously in public (posting, publishing) is a bigger trust
   question than dev-role staging autonomy, deliberately deferred.

---

## 12. Risks & Maturity Assessment

| Risk | Mitigation |
|---|---|
| Drift back into per-aspect agent splitting on one feature | §4 is a locked v1 decision: one agent, one feature, end-to-end. Don't reintroduce sub-task decomposition to "parallelize" a single feature. |
| OpenClaw & Hermes ship no official images / no k8s | Own Dockerfiles; expect daemon-vs-pod lifecycle work; pin versions. Still true regardless of §11.1 outcome. |
| Fast-moving 2026 ecosystem; community sites overstate capabilities | Only first-party GitHub repos are authoritative (§5). |
| Agent runaway spend / token burn | No hard cap in v1 by design (§11.3) — mitigated instead by ledger visibility + Discord/Grafana alerts on anomalous burn, until a cap is worth setting. |
| Privilege escalation via shared repo PVC / Docker socket | One PVC/worktree per task; never mount host Docker socket; prefer AgentTier Sandboxes. |
| Production incident from bug-handling loop itself | Bug fixes follow the same staging→approval→production path as features — no direct-to-prod bypass, even for "obvious" fixes. |
| Fleet accidentally touching cluster ops | §11.4 — fleet must not operate `infra-bootstrap` until that `CLAUDE.md` decision is explicitly revisited. |
| Drift between runtimes | GitOps repo is the recovery point; Argo CD auto-syncs; versions pinned. |

---

## 13. Sources

- OpenClaw — [github.com/openclaw/openclaw](https://github.com/openclaw/openclaw)
- Hermes Agent — [github.com/NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
- Claude Code — `@anthropic-ai/claude-code` (Anthropic)
- AgentTier — k8s-native `Sandbox` CRD (Apache-2.0), referenced via [awesome-cli-coding-agents](https://github.com/bradagi/awesome-cli-coding-agents)
