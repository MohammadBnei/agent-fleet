# ADR-0037: thot is a worker task, not a standing service

**Status:** Accepted
**Date:** 2026-08-11
**Supersedes:** ADR-0035

## Context

ADR-0035 shipped and worked — thot answered real questions about the live
cluster within hours of deploying. The problem wasn't capability, it was
that thot arrived as a **second mental model**: a standing Deployment with
its own `ThotService` gRPC, its own `thot_events` stream, its own bespoke
dashboard page, its own bearer token, and a named exception to the fleet's
hub-and-spoke rule. Alerts, agent questions, and human questions each
needed their own bespoke plumbing, and none of them looked like anything
else the fleet does.

Mohammad's framing, which settled it:

> thot is special in its RBAC and reach. Not in how it's conceived and
> operated.

Everything below follows from taking that literally.

Designed through an architecture interview; the alternatives below were
rejected during it, not reconstructed afterwards.

## Decision

**A thot session is an ordinary worker task.** Same `tasks` row, same
`transcript`, same task-detail UI, same warm/stop/interrupt, same
permission-mode picker, same dispatch loop. Concurrent, because two
investigations should be able to run at once.

Concretely: a worker task on `infra-bootstrap` carrying a `cluster-access`
tool ingredient. **The dispatch path required no changes at all** — the
enabling trick is seeding a real `repos` row, since `dispatch.tick` and
`warmIfIdle` both call `repos.Get` and abort on a miss.

**Cluster privilege moves out of the agent into `thot-executor`** — a
small standing Deployment holding the same `ClusterRole` ADR-0032 already
reviewed. thot session pods hold **zero Kubernetes credentials**; they run
`kubectl` through a shim that forwards argv to the executor.

This restores ADR-0012's founding constraint, which ADR-0035 had
deliberately breached: *the thing holding write-in-git trust never also
holds infra-mutation trust*. It also deletes ADR-0035's hub-and-spoke
exception outright — the sidecar is back to exactly one outbound channel.

### How privilege is bounded

- The provisioner **never handles thot's RBAC**: it doesn't create it,
  doesn't name a ServiceAccount, and doesn't create pods in a privileged
  namespace. The executor's identity is defined in infra-bootstrap's
  gitops, human-reviewed, exactly as before.
- `EXECUTOR_ADDR`/`THOT_AUTH_TOKEN` are injected **only** when the
  `cluster-access` ingredient is present, so a normal repo task never
  carries the executor's token.
- The executor takes `repeated string args`, **never a command string** —
  it spawns the array directly, so shell metacharacters are inert by
  construction rather than filtered.
- **Reads are validated; mutations are a dumb pipe.** The split follows
  "is a human actually in this loop?": `kubectl_read` bypasses
  `canUseTool` by design, so nothing else is checking it and the read-verb
  allowlist earns its keep. A mutation was already approved by a human on
  that exact argv, so a second allowlist would only duplicate the gate.
  RBAC is the outer boundary either way — and note it genuinely grants
  `patch` and `delete`, so the read path could not have leaned on it.

## Alternatives considered

- **Bind the ClusterRole to the thot namespace's `default` SA**, so
  identity comes from where the pod runs — rejected: a privileged
  `default` SA is a well-known anti-pattern, and anything that lands in
  that namespace inherits cluster RBAC.
- **Provisioner sets `serviceAccountName` on thot pods** — rejected: it's
  the same boundary in practice (Kubernetes doesn't RBAC-gate *which* SA a
  pod creator may reference, so pod-create in a namespace lets you assume
  any SA in it), and it hands the provisioner identity-attachment power it
  has never had.
- **A Kyverno/Gatekeeper mutating webhook** attaching the SA from a pod
  label — rejected: no policy engine is installed in this cluster, and
  adding one puts a new cluster-wide dependency and a new failure point
  into *every* pod creation, for one use case.
- **Enumerated typed RPCs on the executor** (`RestartRollout`,
  `DeletePod`) instead of argv passthrough — rejected: loses flexibility
  and invents a second permission model alongside `canUseTool`, when the
  human is already the validator.
- **One standing thot session, Deployment scaled 0↔1** — this would have
  kept RBAC entirely out of the provisioner's pod path, and was genuinely
  attractive. Rejected because concurrent sessions are required.
- **A dedicated THOT pod kind** with no repo or worktree — rejected: adds
  branching through dispatch → grpcserver → pod.go, and forfeits the git
  worktree that makes a durable fix a normal PR.
- **Status quo (ADR-0035)** — rejected: the second mental model is the
  thing being removed.

## Consequences

- **Deletes more than it adds**: `ThotService`, `thot_events`, both thot
  clients, the `ask_thot` MCP tool, the sidecar's second outbound
  connection, the bespoke dashboard page, and the whole `thot/` component
  are gone. One new component (`executor/`) replaces all of it.
- **ADR-0035's hub-and-spoke exception no longer exists.** ADR-0020 point
  5 ("two local entry points, one upstream channel") holds again
  unqualified.
- **thot gains** live transcript streaming, interrupt, the permission-mode
  picker, and mobile support — none of which the bespoke page had.
- **An audit becomes a visible task** rather than an invisible RPC, so it
  can be read, interrupted and resumed like anything else.
- **The durable-fix PR path comes free**: a thot session has a real git
  worktree on infra-bootstrap and the normal `gh pr create` flow.
- `thot_events` is dropped, losing the rows written during thot's first
  day. They were never a system of record — only its own scratch feed —
  and there is no task to attach them to, which is why dropping beats
  migrating.
- A compromised executor is still a serious event: it can read anything
  in the cluster and restart/delete workloads. It cannot read secrets,
  touch RBAC objects, or mutate nodes — the exclusions from ADR-0032 are
  unchanged.

## Out of scope

Alertmanager → thot and the proactive Discord channel. Both get simpler
after this (an alert becomes "create a task"), so they follow rather than
block.
