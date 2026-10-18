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
release tag (`version`, e.g. `4.19.0`). Not `main`: definitions on `main` would
go live within a minute of a merge, judged by an older core whose strict decode
does not know a new field, and a security-relevant change (audience, embed,
credentials, actors) would skip the release gate. At the tag, definitions ship
with the core that judges them, and the PR's CI ran that core. Cost: a
definition-only change goes live at the next release. kind and dev set the
override to a branch.

### 3. `(name, sha)` rows, latest valid wins, revocation

Migration 000008 adds `agent_definitions(name, sha, spec, effective, valid,
error, synced_at)`, PK `(name, sha)`, and nullable
`sessions.definition_name/definition_sha` with a FK to it plus a pair CHECK
(a composite FK is MATCH SIMPLE, so a half-NULL pair would skip it). Rows are
never deleted: sessions pin them.

Core syncs every 60s in its own goroutine (not inside `sessions.Loop`, whose
serial tick a slow fetch would delay). For each non-abstract definition it
reuses any existing row with identical content (bumping `synced_at`), or
inserts one at the current sha. So a pinned sha names the commit where that
content first appeared, and every release commit does not mint a new row.
Invalid definitions are **stored** with their error and logged
(`definitions: invalid`); errors with no row (bad path, abstract parent,
client) are logged and returned to the console as `sync_errors`. A failed
listing keeps the last-known-good state.

Each sync **re-judges** every valid row of a present name under this core's
ceiling and flips it to invalid in place when it no longer passes, so a ceiling
tightening in a core upgrade revokes old rows. The current version of a name
is its **latest valid** row. A merged typo leaves the previous valid version
current (the console shows `newer_error`).

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
`SetPermissionMode` on a definition session must stay within its
`mode.allow`, for admins too: a definition only narrows.

`WarmIfIdle` refuses a definition session unless the provisioner reports
`GetVersion.definition_pod_profile` (ADR-0064). Until that slice lands, every
definition warm is refused **before** `ReserveSlot`: definition sessions can
be created and listed but never get a pod with the ordinary profile.

### 5. Capacity: a pool inside the global cap

`MAX_DEFINITION_SESSIONS` (default 0) must be `>= 0` and `<
MAX_IN_FLIGHT_TASKS`, or core refuses to start. Inside `ReserveSlot`'s
existing advisory-locked transaction, a definition session is also checked
against the pool and against its own `limits.concurrency`, and a session
pinned to a row that is no longer valid is refused. Every check runs before
any write, so a refusal leaves no lease and no phase. Repo sessions never
count against the pool, so a full pool still leaves admins
`MAX_IN_FLIGHT_TASKS - MAX_DEFINITION_SESSIONS` slots.

**Insert + reserve in one transaction is P4, not P1.** The plan's P4 row lists
it, under the AgentService (`Open`/`Invoke`) that creates *and* provisions in
one request. P1 console sessions keep ADR-0048 §1's row-only create; the first
`PostMessage` warms through the transaction above.

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
phase that applies one removes it from this list. `tools.allow` is a shape
rule, not a sandbox (`Bash(go run:*)` passes): it is safe only because it never
bypasses `canUseTool`.

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
