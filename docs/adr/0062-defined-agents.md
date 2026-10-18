# ADR-0062 — Defined agents

- **Status:** Accepted
- **Date:** 2026-10-18
- **Related:** [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §1
  (amended: the gate), [ADR-0061](0061-sessions-launch-in-auto.md) (definitions
  are an exception to its `auto` launch default), [ADR-0069](0069-component-auth-and-network-fences.md)
  (the auth this builds on), ADR-0064 (the definition pod profile, next slice),
  `VISION.md` principles 4 and 8 (amended). infra-bootstrap ADR-0060 (the event
  bridge) shares the gate amendment below.

## Context

Every session today is a *repo session*: an admin picks a repo, a model and a
mode in the console, and the pod gets the fleet's ordinary profile (GH_TOKEN,
the fleet-shared settings, any mode up to `auto`). The plan for "defined agents"
needs sessions whose shape is declared ahead of time and reviewed like code: a
workspace co-editor for the Opus team, event-driven agents behind the bridge.
That shape has to live somewhere a PR can review, be pinned by the session that
uses it, and be checked against a ceiling the fleet owns, so a definition can
only ever *narrow* what a session may do.

P1 builds the registry and the console path. It does not build the vault, the
egress lock, the genome, the AgentService or a CRD (P2 to P4).

## Decision

### 1. Definitions are files in git

`definitions/<name>.yaml` (`apiVersion: fleet.bnei.dev/v1alpha1`,
`kind: AgentDefinition`) and `definitions/clients/<name>.yaml`
(`kind: AgentClient`). Any other path under `definitions/` is a load error
(a `.yml` typo fails CI instead of vanishing). `*.md` is ignored.

- **Strict decode.** An unknown key at any depth, a second YAML document,
  a wrong `apiVersion`/`kind`, or `metadata.name` not equal to the file stem
  is an error. Names match `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, so a name
  is also a valid Kubernetes label value.
- **`extends` is top-level key replace.** A child's spec key replaces the
  parent's whole value. Nothing is deep-merged: a child's `tools` drops the
  parent's `tools.deny`. Cycles and unknown parents are errors; a parent's
  error becomes the child's. `abstract: true` is never inherited, and an
  abstract definition gets no row and is never launchable.
- **Validation runs on the EFFECTIVE spec** against a fleet ceiling in
  `core/internal/definitions/load.go`. Errors are joined, so a row's `error`
  lists every violation. The ceiling table (all rules, required fields):
  audience group in `{platform-admins, shared-notes}`; embed origins exact
  same-site `https://` origins; `credentials.vault == metadata.name`; service
  hosts from a fixed list with no wildcard; github paths exactly
  `/<o>/<r>/*`, `/<o>/<r>.git/*` or `/repos/<o>/<r>/*` (spike 3: the vault
  matches by prefix with no normalisation, so `..`, `%` and extra `*` are
  rejected); `mode.allow ⊆ {default, plan, dontAsk}` (`acceptEdits`, `auto`
  and `bypassPermissions` are rejected by name: they skip `canUseTool`) and
  `mode.default ∈ mode.allow`; `thinking.default` an SDK 0.3.301
  `EffortLevel`; `model.allow` within the console's model list; `tools`,
  `sidecarTools`, `interaction`, `lifecycle`, `resources`, `limits` within the
  ceiling; `owns`/`subscribes` shape-checked only (subjects are bridge globs,
  types are prefixes, passed through as-is).
- **Clients: two-sided agreement.** A client lists the definitions it may
  open, *and* each such definition lists the client in `spec.invokers`.
  Checked at load. Enforced at `Open` in P4; there is no client table in P1.
- **A CI check** (`core/internal/definitions/repo_test.go`) runs the same
  validator over the repo's `definitions/`. `.github/workflows/go.yml` runs
  core's tests on a `definitions/**` change.

### 2. Read at the release tag, through the provisioner

The provisioner already clones this monorepo for fleet-shared. A new
`ProvisionerService.ListDefinitionFiles` reads every regular file under
`definitions/` from its **own** clone (`<root>/definitions-src`, own lock, so
a slow fetch never stalls `CreateWorkerPod`) and returns them with
`git rev-parse HEAD`. It **fails closed**: a missing directory, a symlink, an
oversize file or a fetch failure is an error, never an empty list.

The ref is `FLEET_DEFINITIONS_REF`, defaulting to the provisioner's own
release tag (`version`, e.g. `4.19.0`). Not `main`, and not the fleet-shared
clone at its HEAD: definitions on `main` would
go live within a minute of a merge, judged by an older core whose strict decode
does not know a new field, and a security-relevant change (audience, embed,
credentials, actors) would skip the release gate. At the tag, definitions ship
with the core that judges them, and the PR's CI ran that core. Cost: a
definition-only change goes live at the next release, and a second clone and
lock on the provisioner. kind and dev set the override to the branch under test
(the `kind-local` skill does it after applying the manifest).

**Deviation from the P1 build contract**, which said to read the fleet-shared
clone at its HEAD: that clone follows `FLEET_SHARED_BRANCH` (`main`) and its
lock is taken blocking by `CreateWorkerPod`, so both the release-gate and the
stall argument above apply. Recorded for the reviewer to accept or overturn.

### 3. `(name, sha)` rows, HEAD's row is current, revocation

Migration 000008 adds `agent_definitions(name, sha, spec, effective, valid,
error, synced_at)`, PK `(name, sha)`, and nullable
`sessions.definition_name/definition_sha` with a FK to it plus a pair CHECK
(a composite FK is MATCH SIMPLE, so a half-NULL pair would skip it). Rows are
never deleted: sessions pin them.

Core syncs as the last step of `sessions.Loop`'s 60s pass (no second
scheduler; the loop's ticker does not slip for a pass shorter than the
interval, and the sync has its own 30s timeout). For each non-abstract definition it
reuses any existing row with identical content (bumping `synced_at`), or
inserts one at the current sha. So a pinned sha names the commit where that
content first appeared, and every release commit does not mint a new row.
The same `(name, sha)` judged differently (a core rollout overlap, an upgrade)
never rewrites a **valid** row's content, which sessions may pin: it may only be
flipped invalid, `effective` kept. An invalid row is replaced outright (it is
not launchable; this is how an older core's strict-decode rejection of a newer
field heals once the newer core syncs).
Invalid definitions are **stored** with their error and logged
(`definitions: invalid`); errors with no row (bad path, abstract parent,
client) are logged and returned to the console as `sync_errors`. A failed
listing keeps the last-known-good state.

Each sync **re-judges** every valid row of a present name under this core's
ceiling and flips it to invalid in place when it no longer passes, so a ceiling
tightening in a core upgrade revokes old rows. The current version of a name
is **the row of the content at the synced commit, if it is valid**, never an
older one. An invalid HEAD fails closed (create and warm refused, the console
shows the error): falling back to the previous valid row would let a narrowing
edit that also carries a mistake resurrect the wider version it replaced.

**Revocation rule.** A definition session warms only while its pinned sha is
the current one. Deleting the file, merging a content change, or a core
upgrade that rejects the content stops existing sessions at their next warm
with `FailedPrecondition` ("changed since this session was created … start a
new session"). Any content change ends re-warms of older sessions: they are
cheap to recreate, and a pinned-but-superseded spec is exactly what revocation
has to stop.

### 4. The console path (P1: platform-admins only)

`DashboardService.ListDefinitions` lists each present definition's current
version. `CreateSession` gains an optional `definition`; when set, `repo`,
`model` and `permission_mode` must be empty and come from the definition:
`workspace.repo`, `model.default`, and `mode.default` written **explicitly**,
so the `'auto'` column default (ADR-0061) can never apply. A repo that is not
onboarded is a clean `FailedPrecondition` at create time, not a validation
error (the repo table is a runtime fact). A cluster-access repo is refused.
`SetPermissionMode` on a definition session must stay within its pinned
version's `mode.allow`, and only while that version is valid, for admins too:
a definition only narrows. The guard is in `sessions.Store.SetPermissionMode`'s
own statement, so the console (DashboardService) and the pod (CoreService,
`PermissionDenied`) both go through it, and the console persists before it
appends, so a refused mode never reaches the worker.

`WarmIfIdle` refuses **every** definition warm in this slice, after the
revocation checks and **before** `ReserveSlot`: this core never sends the
profile in `CreateWorkerPod`, so a provisioner-side "supported" flag would be a
promise it cannot keep (a newer provisioner saying yes would still build the
ordinary profile: GH_TOKEN and LEASE_ID in the worker). ADR-0064 replaces the
refusal with a check in the same change that sends the profile. Definition
sessions can be created and listed, never warmed, until then.

### 5. Capacity: a pool inside the global cap

`MAX_DEFINITION_SESSIONS` (default 0) must be `>= 0` and `<
MAX_IN_FLIGHT_TASKS`, or core refuses to start. Inside `ReserveSlot`'s
existing advisory-locked transaction, a definition session is also checked
against the pool and against its own `limits.concurrency`, and a session
pinned to a row that is no longer valid is refused. Every check runs before
any write, so a refusal leaves no lease and no phase. Repo sessions never
count against the pool, so a full pool still leaves admins
`MAX_IN_FLIGHT_TASKS - MAX_DEFINITION_SESSIONS` slots.

**The create and its capacity check are one transaction.** A definition
`CreateSession` runs the same checks (fleet cap, pool, `limits.concurrency`,
row still valid) under the same advisory lock right after its INSERT, and a
refusal rolls the INSERT back: no row is kept. It does not *reserve* (no
lease, no phase): ADR-0048 §1 still provisions on the first message, and a
lease with no pod being created is a state the reconcile loop reads as a
stalled start. The full insert + reserve is P4's `Open`/`Invoke`, which
create and provision in one request; every capacity refusal wraps
`ErrAtCapacity` with which cap was hit.

### 6. The gate amendment (shared with the event bridge)

ADR-0048 §1 and `VISION.md` principle 4 said: a session is provisioned only by
a message a human sent through the console. Amended to: **the provisioning
message comes from an audience member, a registered client, or an
allowlisted actor's event delivered via a registered client.** Principle 8's
"a PR is the intended result" gains the same qualifier for `git.mode: push`
definitions, which the *planned* ADR-0070 (Headless, P4) will decide; in P1
`git.mode` is schema only. In P1 only platform-admins can create any session,
so the de facto gate is unchanged until P4.

### 7. Enforced vs inert in P1

P1 **validates** every rule above but **applies** only: `audience.group`
(create is admin-only anyway), `workspace.repo`, `git.identity` (ADR-0064),
`model.default`, `mode.default` + `mode.allow` (SetPermissionMode),
`limits.concurrency` (ReserveSlot), and `sidecarTools` only indirectly
(ADR-0064's CoreService allowlist is a fixed table, not derived from it).

**Inert in P1** (validated, reach nothing): `tools`, `sidecarTools` as a list,
`model.allow` beyond the default, `thinking`, `systemPrompt`, `skills`,
`interaction`, `lifecycle`, `limits.maxTurns`/`timeout`/`startupBudget`,
`resources`, `credentials`, `embed`, `invokers`, `owns`, `subscribes`. Each
phase that applies one removes it from this list.

`tools.allow` is a shape rule, not a sandbox. Bash entries must be a literal
command prefix (`Bash(git add:*)`): any other `*`, a leading space, or a shell,
exec wrapper or interpreter as the command (`sh`, `bash`, `env`, `xargs`,
`python`, … also by path) is rejected as unscoped Bash by another name; a
denylist cannot be complete (`Bash(go run:*)` still passes), so the reviewed PR
is the real check. **P4 must decide `tools.allow` inside `canUseTool`, never
pass it as the SDK's `allowedTools`**: the SDK documents `allowedTools` as
auto-allowed *without* `canUseTool`, which would break the gate (plan §3,
Worker). Required on every non-abstract spec: `tools` with a non-empty
`allow`, `interaction`, `lifecycle` (absence is no bound; nil `tools` would be
the SDK's default tool set), and an explicit `null` spec value is a load error
(it would erase the parent's bound in the top-level merge).

## Consequences

- A definition change is a reviewed PR plus a release. That is the point.
- **Rollback below this ADR:** `migrate down` drops the pinning, and older code
  would warm definition rows as ordinary repo sessions (GH_TOKEN, any mode).
  **Archive definition sessions before rolling core back.**
- `current` lives in core's memory (core is single-replica). After a restart,
  `CreateSession(definition)` answers `Unavailable` until the first sync, which
  runs immediately.
- New core with an old provisioner: `ListDefinitionFiles` is Unimplemented,
  nothing syncs, definition creates answer `Unavailable`. Repo sessions are
  unaffected.
