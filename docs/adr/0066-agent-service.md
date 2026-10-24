# ADR-0066 — AgentService: one API for members and registered clients

- **Status:** Accepted (2026-10-22) — §1 amended by [ADR-0068](0068-islands.md) (CORS for island origins); §4 amended by [ADR-0076](0076-definition-sessions-run-in-auto.md) (`scope`, `rule_form`)
- **Built:** both listeners (`:8080` members, `:9091` clients with a bearer per registration),
  the fail-closed authz table (`core/internal/agentsvc/authz.go`), definitions, threads,
  messages, transcript, decisions, `SetConfig` (mode), `Stop`, `GetStatus`, and `Open`/
  `CloseThread` with `client_requests` claims; the bridge dispatcher calls `Open` in-process as
  the `bridge` client (#324). **Not yet built:** `Invoke`/`WaitForResult`
  (declared in the proto and the authz table, served `Unimplemented`; ADR-0070); §9's
  per-definition `CoreService` method set (core still uses the fixed `definitionMethods`) and
  sidecar tool filter; §11's SDK helpers (there is no `sdk/`).
- **Date:** 2026-10-20
- **Related:** [ADR-0062](0062-defined-agents.md) (definitions, clients and
  the gate amendment; §5's create + reserve transaction is replaced here),
  [ADR-0063](0063-context-packs.md) (every warm goes through `WarmIfIdle`),
  [ADR-0064](0064-definition-pod-profile.md) (the fixed CoreService allowlist
  this derives from `sidecarTools`; the peer-visibility asymmetry),
  [ADR-0069](0069-component-auth-and-network-fences.md) (the gate, the
  per-request admin check and the fences this extends),
  [ADR-0056](0056-the-console-gate-is-oidc-in-core.md) (the console gate),
  [ADR-0067](0067-shared-and-private-sessions.md) (owner, visibility,
  authorship, the worker's spec), [ADR-0070](0070-headless-invoke.md)
  (headless invoke), ADR-0071 (#257, the bridge's dispatcher, the main
  client). infra-bootstrap #342 (`sub_mode: hashed_user_id`).

## Context

Since P1 every definition session is created by a platform-admin in the
console. The plan needs two other callers:

- **Members**: people in a definition's audience group (`shared-notes` for
  `opus-workspace`) who are not platform-admins. They drive a session from an
  embedded widget (ADR-0068, P5).
- **Clients**: registered machine callers (`definitions/clients/*.yaml`):
  the event bridge's dispatcher (ADR-0071) and apps that invoke a definition
  headlessly (ADR-0070).

Today none of that is possible, and none of it is checked. Verified on main
at 7c6f1a5:

- Core has one public principal. `auth.AdminOnly` gates the whole `:8080`
  mux (`OIDC.Gate`, `core/internal/auth/gate.go:80`) and every Connect call
  (`ConnectInterceptor`, `gate.go:177`), wired once at `run.go:326-330`. The
  OIDC callback refuses anyone outside `platform-admins` (`oidc.go:208`).
- No `DashboardService` handler checks who owns a session. Admin-only is the
  whole authorization.
- `audience.group` and `invokers` are validated and never compared with a
  caller (ADR-0062 §7). Client files are parsed (`load.go:116-122`);
  `tokenRef` is checked only as a key name, and nothing reads it.
- `CoreService`'s definition allowlist is one fixed map for every definition
  (`coreserver/auth.go:142-152`). The sidecar registers every MCP tool
  (`sidecar/internal/mcpserver/server.go:89-200`).
- The cookie carries `Sub`, but its comment says it is not stable yet
  (`session.go:62-73`). infra-bootstrap #342 switched authentik's fleet
  provider to `hashed_user_id`; cookies minted before that carry the email.
- **Warm, then append** (`server.go:955-996`). The new pod's cursor is
  `LatestSeq` read at warm time, so an entry appended to a cold session is
  never delivered. `WarmIfIdle` is the single pod path, and it renders the
  context packs (ADR-0063 Decision 4).
- **A pinned definition session cannot warm after its definition changes.**
  `definitionWarmable` refuses when the pinned sha is not the current head
  (`server.go:1212`). The idle sweep can also mark a session swept, which
  `WarmIfIdle` refuses.

The widget (P5) and headless apps (P7) must use the same contract. If the
widget had a private API, the SDK would be the second-class one.

## Decision

### 1. One service, two listeners, the principal comes from the listener

`proto/agentfleet/v1/agent.proto` declares `agentfleet.v1.AgentService`. One
handler set is mounted twice:

- **`:8080`, members.** On the existing mux, behind `OIDC.Gate`, `FrameDeny`
  and a CSRF header check like `DashboardService`'s. The widget is an iframe
  served from the fleet's own origin (ADR-0068), so its calls are
  same-origin. **Core never sends CORS headers**, for this service or any
  other. *(Amended by [ADR-0068](0068-islands.md): islands are web components,
  not an iframe. Core answers CORS on the `AgentService` prefix for the exact
  origins in valid definitions' `embed.origins`, with credentials; every
  other path still sends none.)*
- **`:9091`, clients.** A new `http.Server` with only `AgentService` on it.
  ClusterIP only: the Service gains port `9091`, and no IngressRoute,
  Traefik route or `:8080` path ever reaches it.

The handler reads the principal from the context, set by the listener's
interceptor: `member{sub, email, name, groups}` or `client{name}`. With no
principal in the context (for example `FLEET_AUTH_DISABLED=1` in kind),
every AgentService handler refuses with `Unauthenticated`. A client token
sent to `:8080` is not a cookie and is refused. A cookie sent to `:9091` is
ignored.

`AgentService` returns its own messages (`Thread`, `Entry`, `Status`), never
`DashboardService`'s `Session`. Pod internals (`pod_message`, pod names,
`agent_session_id`, leases, images) are not in them.

### 2. Members: login widens, the gate does not

- **Login.** The callback admits any group in the definitions ceiling's
  `knownGroups` (`load.go:26`: `platform-admins`, `shared-notes`).
  `RequiredGroup` becomes `AdmitGroups`. Group membership is still not
  copied into a user table.
- **The `:8080` gate stays `AdminOnly` by default.** A member reaches only
  an explicit allowlist of paths: the `AgentService` path prefix, `/auth/*`,
  and later the `/islands/*` prefix and the exact path `/auth/done`
  (ADR-0068, which drops the planned `/embed/*`). Any route added later is admin-only unless
  someone adds it to that list. `DashboardService` keeps its own `AdminOnly`
  interceptor as a second layer, and ADR-0069's test matrix gains a member
  identity that gets `PermissionDenied` on every `DashboardService` method
  and `403` on `/`.
- **`AgentService` on `:8080`** admits a member of any known group, then the
  per-method table (§4) decides per definition and per session.
- **`sub` is the owner key.** A cookie with an empty `Sub` is refused by
  `AgentService` with `Unauthenticated`, so the widget re-logs in. Rollout
  step 1 rotates `SESSION_KEYS`, so no cookie minted before #342 (when `sub`
  was the email) survives. The email and name are display only.

### 3. Clients: a bearer per registration, fail closed

- **Token.** `definitions/clients/<name>.yaml` `tokenRef` names an env var
  in core (`FLEET_CLIENT_TOKEN_<NAME>`, matched by the validator). Core's
  InfisicalSecret on `agent-fleet-nygh` syncs it. The client app reads the
  same value from its **own** Infisical project, never `platform-commons`.
- **In-process clients have no token.** The dispatcher (ADR-0071) runs
  inside core and calls the handlers with the client principal in-process.
  This ADR makes `tokenRef` optional in the validator (it is required today,
  `load.go:346`) and removes it from `definitions/clients/bridge.yaml`. A
  registration without `tokenRef` is in-process only: it never matches a
  bearer on `:9091`, so its wake rights cannot be reached from the network.
- **Check.** `Authorization: Bearer <token>`. Core compares it in constant
  time against **every** enabled client's token (no early exit), and the
  match names the client. The client must also be registered in the current
  sync. A token shorter than 32 bytes, or an empty env var, **disables** that
  client; this is logged at startup and on each sync, and is never an open
  client. An empty bearer is refused before any compare.
- **Rotation.** The env var may hold two comma-separated tokens, so the old
  and the new value both work until the client switches. Core reads env at
  start, and nothing restarts it when the Secret changes today (core's
  InfisicalSecret sets no auto-reload). Rollout step 4 adds the reload
  annotation, so removing a token takes effect when the operator syncs and
  core restarts. Until then, revocation is a manual restart.
- **Fence.** The core ingress NetworkPolicy (ADR-0069) admits `:9091` only
  from namespaces labelled `fleet.bnei.dev/client`. That excludes
  `agent-fleet` itself, so worker and definition pods cannot reach `:9091`.
- **No client acts as a member.** The client principal has no `sub` and no
  groups.

### 4. A per-method authorization table that fails closed

`core/internal/agentsvc/authz.go` holds one Go map from full method name to a
rule for each principal. A method missing from the map is refused. A test
walks the `AgentService` descriptor and fails when any method has no entry,
so a new RPC cannot ship unauthorized.

| Method | Member | Client |
|---|---|---|
| `ListDefinitions` | definitions whose audience the member is in (admins: all) | definitions it may open (§5) |
| `ListThreads` | threads the member may see (ADR-0067 §2) | its own threads |
| `CreateThread` | audience member of the definition | not allowed (clients use `Open`) |
| `PostMessage` | may answer the session (ADR-0067 §2): the same rule as answering, so never into another member's or a client's private session, nor a tainted one | not allowed |
| `Open`, `CloseThread` | not allowed | definition it may open, own threads |
| `Invoke`, `WaitForResult` | not allowed | definition it may open, own sessions (ADR-0070) |
| `GetTranscript`, `StreamTranscript`, `GetStatus` | may see the session (ADR-0067 §2) | own sessions |
| `RespondToPermission`, `AnswerQuestion`, `ApprovePlan` | may answer the session (ADR-0067 §2) | own sessions with `interaction: ask` that are not tainted |
| `SetConfig`, `Stop` | session owner, or admin | own sessions |

- "Admin" means `platform-admins` in the cookie, checked on the same request.
- **Every session-scoped method refuses a session with no
  `definition_name`.** Repo sessions stay `DashboardService`-only.
- **One answer for "not yours" and "does not exist".** A session the caller
  may not see gets `NotFound`, the same as a random id, so private threads
  cannot be enumerated.
- **Replies are bound to their request.** `RespondToPermission`,
  `AnswerQuestion` and `ApprovePlan` must name the `seq` of an unanswered
  request of that session's own transcript, of the matching type. Migration
  000015 adds a unique partial index on `(session_id, reply_to_seq)` for
  answer and permission-response entries, so two members answering one
  prompt at once produce one answer and one `AlreadyExists`.
- **A permission answer is `{behavior, message}` and nothing else.** Core
  parses it and drops every other field before appending. A member cannot
  answer "allow" with `updatedInput` (a rewritten command) or
  `updatedPermissions` (a standing allow rule). The worker also ignores those
  fields from any definition session's answer.

### 5. Who may open a definition is decided on the current sync

A client may open a definition when the client file lists it **and** the
definition's current head lists the client in `invokers`. A member may use a
definition when they are in its current head's `audience.group`, or are an
admin. Both come from the registry's current sync, never from the pinned
row's `effective` (ADR-0062: invokers are not re-judged on stored rows).

- The registry is core's in-memory last good sync. After a restart, calls
  answer `Unavailable` until the first sync, which runs at once (ADR-0062).
- An invalid head, or a definition the sync no longer has, refuses new
  threads and new messages.

### 6. `Open`: resume or create a thread, with the client's trust decision

`Open(definition, thread_key, message, wake, tainted, interaction,
lifecycle, visibility, idempotency_key, actor, causation_id)` is the
dispatcher's entry point (ADR-0071). `tainted` marks the message untrusted
(ADR-0067 §3).

- **Threads are per client.** A unique partial index on `(invoker,
  definition_name, thread_key) WHERE archived_at IS NULL` names at most one
  open thread per client and key. One client cannot squat or probe another's
  keys.
- **Trust is the client's decision, bounded by core.** ADR-0071 computes
  trust in the bridge (PR author plus same-repo head, event caps, proposals
  for untrusted openers). `Open` carries the outcome as `wake`:
  - `wake: true` warms (if needed) and appends a waking message. Core also
    requires `actor` to be in the client's `actors.allow`, which is a
    ceiling on what the client may claim. The match is exact and fails
    closed. `actor` is a GitHub login for GitHub events, and
    `agent:<definition>` for an event an agent published through
    `PublishEvent`. An agent hand-off that may wake a session therefore needs
    its own `agent:<definition>` entry in `actors.allow`; an empty or unknown
    actor never wakes.
  - `wake: false` appends the message `context_only` (ADR-0067 §3) and never
    warms.
  - A **new** thread needs `wake: true`. A `wake: false` `Open` with no open
    thread gets `FailedPrecondition`, and the dispatcher files a proposal
    instead (ADR-0071 §6).
  - `tainted: true` stores the message untrusted (ADR-0067 §3) on every
    `Open`. **On an existing thread, core forces a tainted message to
    `context_only`**, whatever `wake` says, so an untrusted follow-up never
    wakes. Only a thread's opening message may be tainted and still wake,
    because the bridge has already decided its opener is trusted
    (ADR-0071 §6). That session is tainted from its first entry.
  - The 3-session event cap and "a session blocked on a human gets
    follow-ups context-only" are the dispatcher's rules (ADR-0071 §6), not
    `Open`'s.
  - **A capacity refusal is not an event failure.** `Open` answers
    `ResourceExhausted`. The dispatcher turns it into a proposal or a delayed
    retry (ADR-0071 §6), and never holds the bridge's head of line on it.
    With unwatched `ask` threads able to hold a pool slot for hours, this
    refusal is routine.
- **Per-thread choices.** `interaction`, `lifecycle` and `visibility` are
  required on create (no defaults), and `interaction` and `lifecycle` must be
  in the definition's `allow` lists. Mode is the definition's
  `mode.default`, never the `auto` column default (ADR-0061). An `Open` that
  resumes with different values gets `FailedPrecondition`.
- **A thread that cannot warm is replaced.** When a waking `Open` finds the
  open thread unwarmable (its pinned sha is no longer the head, or it was
  swept), core archives that thread and creates a new one with the same key.
  The new thread's first entry names its predecessor. A definition edit
  therefore starts fresh threads instead of wedging the bridge's cursor.
- **`CloseThread(definition, thread_key)`** archives the open thread. The
  bridge calls it when a PR is merged or closed, or an alert resolves.
- **Member threads are not replaced automatically.** A `PostMessage` or
  `AnswerQuestion` on a member thread whose definition changed answers
  `FailedPrecondition` with the reason `DEFINITION_CHANGED`. The widget then
  offers to continue in a new thread, which links the old one (ADR-0062: "start
  a new session"). A person decides; nothing is silently moved.

### 7. Requests are claimed before any warm; nothing is ever deleted

ADR-0062 §5 asked `Open`/`Invoke` for one insert-and-reserve transaction.
Pod creation is not in Postgres, so no transaction can span it, and a
compensating delete races with concurrent callers on the same row. Instead,
every `Open` and `Invoke` claims its idempotency key first:

- **`client_requests(invoker, idempotency_key, session_id, request_sha,
  state, claimed_at)`**, primary key `(invoker, idempotency_key)`. Rows are
  never deleted, so a key stays used across archive: a redelivered event for
  a closed thread is answered from its claim, and nothing runs twice. For the
  bridge the key is the JSON array `["<source>","<id>"]`.
- **Serialization, without waiting.** The request checks out one pooled
  connection and takes a session-level `pg_try_advisory_lock` on a hash of
  `(invoker, idempotency_key)`, and for `Open` a second one on `(invoker,
  definition, thread_key)`. If either is held, it answers `Aborted` ("in
  progress, retry") at once and holds no connection. Nothing ever blocks in
  the database waiting for a lock, so a burst on one thread cannot use up
  core's pool. The holder keeps both locks until it answers. It unlocks with
  a fresh background context, and if the unlock fails it closes that
  connection instead of returning it to the pool. A core crash drops the
  connection, which releases both locks. Every write below commits at once
  on its own, so nothing waits on an open transaction across the warm.
- **Order, under the locks:**
  1. Read the claim, or insert it `pending`.
  2. **`done`:** return its session, with no warm and no append. The
     redelivery that ADR-0071 retries is free.
  3. **Same key, different `request_sha`** (a hash of definition, thread_key
     and message, or of definition and input for `Invoke`):
     `FailedPrecondition`.
  4. If the claim names a session whose transcript already holds an entry
     with this key, the earlier attempt delivered but crashed before step 5
     finished: mark the claim `done` and return that session. Otherwise
     resolve the session: the claim's session if it is still open; for
     `Open`, otherwise the thread's open session (§6 replaces an unwarmable
     one); otherwise create a row. Record the id in the claim.
  5. `WarmIfIdle` if waking. Then append with the key as the transcript
     idempotency key **and mark the claim `done` in the same transaction**,
     so "delivered" and "claim done" can never disagree.
- **Any failure leaves the claim `pending`** and the session row in place,
  with no pod. A capacity refusal (`ResourceExhausted`), a definition
  refusal, a cancelled context and a core crash all end the same way. The
  client's retry with the same key runs steps 4 and 5 again. If the claimed
  session was archived meanwhile (swept, closed, or torn down after a
  failure), step 4 picks or creates a new one and repoints the claim, so a
  key can never be stranded on an archived row. `WarmIfIdle` is a no-op on a
  live pod, and the transcript key makes the append exactly-once.
- **Consequence for ADR-0062 §5:** a capacity refusal can leave a row with no
  pod and no entries. It holds no slot and no lease. Such rows are listed as
  "not started" until a retry warms them. Today's idle sweep only looks at
  rows with a live pod, so the sweep gains one rule: a row with no pod, no
  transcript entry and a `created_at` older than one hour is archived. A
  later retry then repoints its claim (step 4).
- `ResourceExhausted` from `Open` or `Invoke` carries the session id in its
  error details, and the SDK helpers retry the call with the same key and a
  backoff. They never just wait on a session that has not started.
- **Members** do not send keys. `CreateThread` provisions nothing
  (ADR-0048 §1), and `PostMessage` keeps today's warm-then-append with a
  server-minted key.

### 8. Every warm is `WarmIfIdle`

`PostMessage`, `AnswerQuestion`, `Open` and `Invoke` call
`dashboard.Server.WarmIfIdle`, which stays the only path to a worker pod
(ADR-0063's invariant). `AgentService` has no second warm path, so context
packs, the definition profile and the vault checks apply with nothing extra.
**Capacity.** `ReserveSlot`'s definition pool (`MaxDefinition < MaxLive`)
stays. Inside it, sessions owned by members or clients get their own cap,
`MaxExternal < MaxDefinition`, and a member may hold at most `MaxPerMember`
(default 1) live sessions. **A definition's `limits.concurrency` now counts
only member and client sessions.** Admin-created definition sessions draw
only on `MaxDefinition`. Members and clients therefore cannot take a
definition, the definition pool, or the repo slots away from an admin.

A member or client session waiting on an unanswered permission is still
idle-swept once `limits.timeout` passes. Today's sweep skips any session with
a pending permission, on every branch; for these sessions the skip no longer
applies to the `limits.timeout` branch. So a thread nobody answers gives its
slot back.

**No message reaches a worker that has not applied the spec.**
`GetSession` returns the `AgentSpec` with `agent_spec_sha`, the hex sha256 of
its deterministic encoding. After applying the spec (ADR-0067 §4), the worker
echoes that sha on every `StreamHumanMessages` and `WaitForMessages` (the
sidecar keeps the one the worker sent on its stream for the agent's
`wait_for_messages`). For a definition session core refuses both with
`FailedPrecondition` unless the sha equals the one it computes from the pinned
row now. Nothing is stored, so a core restart changes nothing. A worker that
predates this ADR sends no sha, so it never receives a message, whoever warmed
it: a member, a client or an admin through the console. Its session shows
`PROVISION_FAILED` once the startup budget passes. The gated methods are the
three that hand a worker human input, `StreamHumanMessages`, `WaitForMessages`
and `AskUserQuestion` (its answer, so it carries the sha too); the gate is a
rollout gate against a stale worker, not a boundary against the agent, which
holds the sha its own worker was given.

### 9. `CoreService` and the sidecar follow `sidecarTools`

- `GetSessionResponse` gains the session's `AgentSpec` (ADR-0067 §4).
- `coreserver/auth.go`'s fixed `definitionMethods` becomes a per-definition
  set, computed from the pinned row's effective spec on each call:
  - **A base set every definition pod needs:** `SendMessage` (the worker
    writes every agent entry, permission request and result through it, via
    the sidecar's local API; it keeps its `from ∈ {agent, worker}` check),
    `GetSession`, `SaveAgentSessionId`, `PushToolTelemetry`,
    `StreamHumanMessages`, `UpdateSessionMeta`, and `SetPermissionMode` with
    its `mode.allow` check.
  - **Plus each listed tool's methods:** `wait_for_messages` →
    `WaitForMessages`, `AskUserQuestion` → `AskUserQuestion`, and later
    `publish_event` → `PublishEvent` (governed by ADR-0071).
- The sidecar registers only the MCP tools in `sidecar_tools`. Core enforces
  the list for the tools that have their own method (`AskUserQuestion`,
  `WaitForMessages`, `PublishEvent`). `send_message` and `set_session_meta`
  ride on base-set methods the worker itself needs, so for those two the
  sidecar's filter is the only bound. That is acceptable because both write
  only to the pod's own session, as `from` agent or worker, which the agent
  can already do through its normal output.
- `PromptSession` into a definition session is already refused
  (`interagent.go:101-104`). `ListPeerSessions` and `WaitForSessionState`
  now also stop returning definition sessions to repo sessions, which closes
  ADR-0064's recorded asymmetry before audiences exist.

### 10. Status is derived from what is already stored

`GetStatus` returns `loading | working | waiting | idle | error(code)`. It
derives this from `pod_phase`, `DeriveLiveState`, `last_error` and a new
`pod_reason` column (migration 000015), which holds the last pod event's
reason. There is no status column (ADR-0048 §2): these are the facts the
failing paths already record, and the status is computed from them.
`ReserveSlot` clears `last_error` and `pod_reason` when it succeeds, so a
refusal that a retry overcame does not stick. `CAPACITY` is not terminal:
`WaitForResult` keeps answering `running` until the session ends (ADR-0070).

| Code | Written where |
|---|---|
| `CAPACITY` | the warm writes `last_error` with this code when `ReserveSlot` refuses |
| `PROVISION_FAILED` | `CreateWorkerPod` or a definition check refuses (`definitionWarmable`, `refuseDefinitionPod`, today `CRASHED` with a message) |
| `IMAGE_PULL`, `OOM` | a container waiting/terminated reason in the pod event |
| `STARTUP_STALL` | derived: no `RUNNING` within `limits.startupBudget` of the warm |
| `AGENT_CRASHED` | the worker container exited non-zero, with no deadline condition |
| `TIMEOUT` | the Job's `DeadlineExceeded` condition, or the idle timeout ended the pod |
| `CREDENTIAL_STALE` | the vault serving the session (its definition's vault, or its owner vault under ADR-0075) reports a pushed age over its threshold in `ReconcileVaultsResponse`; core keeps that per-vault age in memory |

`PodEvent` (`core.proto:39`) gains `reason`, filled by the provisioner from
container statuses and Job conditions, and stored in `pod_reason`. A core startup histogram (warm →
`RUNNING`) measures `startupBudget` against reality (plan spike 7).

### 11. The SDKs are generated, the helpers are thin

`buf generate` already emits Go and Connect-ES for any proto in
`proto/agentfleet/v1/` (`proto/buf.gen.yaml`). On top of that:

- **TS:** `watchSession` (a `StreamTranscript` reader that folds entries
  into state) and `invokeAndWait`.
- **Go:** `InvokeAndWait`.

They live in `sdk/ts` and `sdk/go`. The widget (P5) imports only the TS SDK.
An ESLint import rule keeps embed code off `DashboardService`.

## Alternatives considered

- **Extend `DashboardService` with member checks.** One service with a
  per-method admin/member split puts the admin console's surface one
  forgotten check away from members. A separate service with its own table
  makes "a member can reach this" an explicit row.
- **One listener for both principals.** Sniffing cookie versus bearer on one
  port makes a mistake in the sniff a principal confusion. Two listeners make
  the principal a property of the socket.
- **Widen the gate's default to members.** It was the first draft. Every
  route added later would have been member-reachable unless its author
  remembered otherwise. An allowlist fails closed.
- **A compensating delete on capacity refusal.** It was the first draft. A
  concurrent `Open` on the same thread can append to the row in between and
  lose its entry to the cascade, and a timed-out request cancels its own
  delete. Claim rows plus retry-on-the-same-session converge without
  deleting anything.
- **Client tokens in a table, hashed.** Better for many clients. Two are
  planned (one over the network). Env vars from Infisical match how every
  other core credential arrives. A table is the upgrade when clients come
  and go without a restart.

## Consequences

- **Members can log in**, and reach only the AgentService path and `/auth/*`.
- **Group membership is up to 12h stale.** Every check reads the cookie's
  groups, so a member removed from `shared-notes`, or a demoted admin, keeps
  their access until the cookie expires, and there is no per-user
  revocation. For `opus-workspace` that includes approving pushes to `main`.
  Accepted for slice 1. The fix, if a removal ever needs to take effect
  sooner, is a shorter TTL for non-admin cookies, plus rotating
  `SESSION_KEYS` to log everyone out at once.
- **Rotating `SESSION_KEYS` logs every admin out once** (rollout step 1).
- `:9091` is a new surface, ClusterIP-only, fenced by namespace label, and
  serving only `AgentService`. Tokens cross the cluster network in plaintext,
  like every other in-cluster call today.
- A stolen client token lets its holder open that client's definitions,
  within their audience, caps and tool surface, until it is rotated in both
  places. It also lets the holder claim any `actor` in `actors.allow`.
- `client_requests` grows by one row per request and is never pruned.
  Pruning rows older than the bridge's retention is the upgrade if it
  matters.
- The bridge's dispatcher (ADR-0071) is unblocked: `Open`, `CloseThread`,
  the client principal and `thread_key` exist.

## Rollout

1. Rotate `SESSION_KEYS` (operator; logs everyone out).
2. The context-packs skills mount (ADR-0063's follow-up PR), then the worker
   applying `AgentSpec`, echoing `agent_spec_sha`, and core's stream gate (§8). No
   caller changes yet: admins still create every definition session.
3. Migration 000015 (ADR-0067), the proto, the handlers behind the authz
   table, and the gate allowlist. The member login widening ships in this
   PR, with the member test against `DashboardService` and `/`.
   Shipped in three PRs: migration 000015 (#289); then the proto, the
   authz table, the gate allowlist, the login widening and
   `ListDefinitions`, with every other method `Unimplemented` behind the
   table; then the thread, message, decision and control handlers with the
   §8 caps.
4. `:9091`, client tokens with the reload annotation, the NetworkPolicy
   port, `CoreService` and sidecar derivation, `GetStatus`.
5. SDK helpers. The QA gate is the plan's P4 row: six sequential invokes,
   then a seventh, with no wedge; a bad image shows `IMAGE_PULL`;
   `/dashboard-e2e` stays green.
