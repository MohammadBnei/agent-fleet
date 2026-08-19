# ADR-0057 — `CoreService` authenticates with the session's own lease

- **Status:** Accepted
- **Date:** 2026-08-19
- **Related:** [#200](https://github.com/MohammadBnei/agent-fleet/issues/200)
  (reported against the dashboard port; this is the wider half),
  [ADR-0056](0056-the-console-gate-is-oidc-in-core.md) (the console half),
  [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §2 (where `lease_id`
  comes from)

## Context

Port 9090 authenticated **nobody**. Any pod in `agent-fleet` could call
`CoreService`, and every method naming a session took that name from the request
body on trust. So a worker pod could act as any other session: read its human
messages, answer its permission prompts, rewrite its metadata, save an agent
session id over its resume identity.

#200 reported this against `DashboardService` on 8080. The gRPC port is the same
hole, wider: `DashboardService` at least had a CSRF header in the way.

This matters more than it looks because the fleet's whole permission design
assumes an agent's authority is what `canUseTool` grants it
([ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md)). An RPC surface
reachable from the pod's own `Bash`, naming any session it likes, sits entirely
outside that.

## Decision

1. **The credential is the session's existing `lease_id`.** `ReserveSlot` mints
   `gen_random_uuid()` on every warm, the provisioner injects it into the pod,
   and `SaveAgentSessionID` already refused a stale one. The sidecar sends it as
   gRPC metadata; core validates `(session_id, lease_id)` against the `sessions`
   table — the same predicate that column already served.

   Reusing it beats minting anything new on every axis that matters. It is
   **random**, not derived from a session id that gets published in Discord deep
   links. It is **per-pod**, not per-session-forever. It **rotates on every
   warm**. It lives only in the component that holds the database
   ([ADR-0020](0020-core-owns-postgres-and-commands-the-provisioner.md) point 1).
   And it requires **no key handed to the provisioner**, which already holds
   `GH_TOKEN` and the fleet's only cluster RBAC.

2. **`ClearLease` at teardown.** Without it the lease stayed valid until the
   *next* warm rotated it — for a session nobody warms again, forever. This is
   what makes it a revocable credential rather than merely a rotating one. Both
   teardown paths call it.

3. **An explicit per-method authorization table, never a generic field lookup.**
   A rule like "find the field called `session_id`" is wrong on three methods
   here, each time in the direction that *grants* authority:

   | Method | Trap |
   |---|---|
   | `PromptSession` | carries `caller_session_id` **and** `target_session_id`, both strings. Binding the target authorises exactly what this prevents. |
   | `SaveAgentSessionId` | carries `session_id` **and** `agent_session_id`. The SDK's conversation id is not a fleet session id — this repo has already shipped a bug from confusing that pair. |
   | `GetSession` | calls it `id`, so a name-based rule silently leaves it unbound. |

   A method in no table **fails closed**, and a test walks the proto descriptor
   requiring every RPC to appear in exactly one. Adding an RPC is now a decision
   about what it may reach rather than a silent grant.

4. **A stream interceptor, not only a unary one.** The server had *only*
   `ChainUnaryInterceptor`. A unary-only check would have left
   `StreamHumanMessages` — the live feed of everything a human types to a
   session, permission answers included — and `ReportPodEvents` wide open, with
   every test still green.

5. **`FLEET_PROVISIONER_TOKEN` is the one shared secret**, for the one caller
   that cannot hold a lease: the provisioner exists before any pod does, and
   `ReportPodEvents` reports on arbitrary sessions by nature — it is how core
   learns a pod exists at all. Unset, it authenticates **nobody** rather than
   everybody, and both processes warn loudly at startup.

## Consequences

**Two methods stay deliberately un-narrowed**, written down rather than
discovered later: `AppendJournal`/`SearchJournal` are repo-scoped and `ViewLogs`
is component-scoped, so a live session can still read fleet-wide. Narrowing them
is a product decision about what a peer session may see, not part of closing an
authentication hole. They now at least require a live lease, where before they
required nothing.

**A misconfigured provisioner token fails silently downward.**
`PERMISSION_DENIED` is not in either client's `retryServiceConfig`, so it is not
retried — it is warn-and-dropped, and the symptom is pod-lifecycle events simply
ceasing to reach core while every log on both sides stays green. Hence the
startup warnings; they are the only loud thing in that path.

**A per-RPC database lookup.** One indexed primary-key read per call, no cache,
marked `ponytail:` at the call site. At `MAX_LIVE_SESSIONS` of 5 that is not
worth a cache and its invalidation bug; the RPC durations the access log already
records are where to look if that changes.

**Deployed but unexercised at the time of writing.** Live on 4.6.2 with zero
auth rejections in the logs — which proves nothing, because there were also zero
worker pods and zero `CoreService` calls in the window. What actually proves
this is the first real session: the sidecar being accepted with its lease, and a
call carrying **another session's** valid lease being rejected. The second is
the case a shared fleet-wide token would have passed, and it is the one worth
checking.

**A shipped compile break, recorded because nothing that ran was wrong.** This
ADR's local `auth := coreserver.NewAuthenticator(...)` and
[ADR-0056](0056-the-console-gate-is-oidc-in-core.md)'s `auth` package import
collided on `main`. Separate PRs, both green on their own branches, neither
branch containing the other's half, **no textual conflict** — so the merge was
clean and the build broke at the release. This PR *was* rebased before merging,
because git flagged a real conflict in `pod.go`; the shadowing produced no
conflict, so nothing prompted a rebase of the other. **Mergeable is not
compiles.** Renamed to `grpcAuth`, which also reads better — it is the gRPC-side
authenticator specifically.

## Alternatives considered

- **A new HMAC-derived per-session token** (`HMAC(key, session_id)`). Rejected
  during adversarial review: `session_id` is not secret — it is in Discord deep
  links, the dashboard URL and the transcript — so the token would be a static
  bearer derived from a public identifier, with no expiry, no per-pod binding
  and no revocation. It also required distributing the signing key to the
  provisioner. `lease_id` is strictly stronger on all four counts and already
  existed.
- **One shared fleet-wide service token.** Rejected: a leaked worker token is
  every session's token, and a worker pod could still impersonate another
  session — the exact case Decision 3's table exists to refuse.
- **NetworkPolicy only.** Rejected as the whole answer, for the same reason as
  in [ADR-0056](0056-the-console-gate-is-oidc-in-core.md): it constrains
  topology rather than establishing identity, and gives no attribution. Still
  worth having underneath.
- **Kubernetes ServiceAccount tokens + `TokenReview`.** Rejected: core
  deliberately holds no cluster RBAC ([ADR-0020](0020-core-owns-postgres-and-commands-the-provisioner.md)
  point 1), and `TokenReview` would require it.
