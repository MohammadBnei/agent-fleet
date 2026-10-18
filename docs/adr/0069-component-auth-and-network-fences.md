# ADR-0069 — Component auth, network fences, console CSP, per-request admin check

- **Status:** Accepted
- **Date:** 2026-10-18
- **Related:** [ADR-0020](0020-hub-and-spoke-grpc-worker-sidecar.md) point 4
  (amended here: network reachability stops being the provisioner's only
  boundary), [ADR-0037](0037-thot-is-a-worker-task.md) (the executor),
  [ADR-0056](0056-the-console-gate-is-oidc-in-core.md) (the console gate),
  [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md) (the
  `CoreService` half, which this mirrors)

## Context

Defined agents (ADRs 0062–0070 and 0073) widen who can start a session and, later, put
a public webhook in front of core. Before that, five gaps that were tolerable
for one admin on a private fleet have to close. All five were verified in code
on 2026-10-18:

1. **`ProvisionerService` authenticates nobody.** The gRPC server on the
   provisioner's `:9090` had only an access-log and a metrics interceptor. Any
   pod in any namespace that could reach the Service could call
   `CreateWorkerPod`, `ExposeSession` (publish a pod to the internet) or
   `ProvisionService` (receive a DSN). ADR-0020 point 4 leaned on "core is the
   only caller", which is a statement about the code, not about the network.
   ADR-0057 closed the opposite direction (provisioner → core) and left this
   one open.
2. **The executor fails open.** `executor/cmd/executor/main.go` passed every
   call when `THOT_AUTH_TOKEN` was empty, with a warning, and compared the
   bearer with `!=`. This is the most privileged process in the fleet (it holds
   the cluster RBAC, ADR-0037). The Infisical operator rendering an empty
   Secret is a known failure mode (ADR-0056's reason for fail-closed startup),
   and here it would come up wide open and look healthy.
3. **There is no NetworkPolicy in `agent-fleet`.** `k8s/provisioner/` used to
   hold one for e2e-runner pods; it went with the sandbox (ADR-0048 §6), and
   `CLAUDE.md`, `docs/ARCHITECTURE.md` and `k8s/provisioner/README.md` still
   claimed it existed.
4. **Core sends no CSP.** Nothing stops another origin framing the console.
   ADR-0068 is about to make embedding a deliberate feature, so the default has
   to become "nobody may frame this" first.
5. **The `platform-admins` group is checked once, at the OIDC callback**
   (`core/internal/auth/oidc.go`). A cookie stays valid for its lifetime after
   the user leaves the group.

## Decision

### 1. `ProvisionerService` requires a bearer token (two PRs, see Rollout)

- A unary **and** stream server interceptor
  (`provisioner/internal/grpcserver/auth.go`) requires
  `authorization: Bearer <FLEET_CORE_PROVISIONER_TOKEN>` on every call.
  Missing metadata, a missing header, a wrong scheme or a wrong token is
  `Unauthenticated`. The comparison is `crypto/subtle.ConstantTimeCompare`.
  The service has no streaming RPC today; the stream interceptor is there so
  the first one added is not silently open, which is exactly the hole
  ADR-0057 found in core.
- **An empty token refuses to start.** `grpcserver.NewServer("")` returns an
  error and the provisioner exits before it listens, the same fail-closed
  startup ADR-0056 chose for OIDC, for the same reason. `main` builds its
  server with that function, and so do the tests, so the tested interceptor
  chain is the shipped one.
- The interceptor is method-agnostic: every method, including ones added
  later, needs the token. There is no exempt list to forget to extend. The
  test enumerates every `ProvisionerService` method from the generated
  descriptor. An unregistered method never reaches an interceptor: grpc-go
  answers it `Unimplemented` first, with no handler behind it.
- Core's `provisionerclient.New(addr, token)` attaches the header with
  `grpc.WithPerRPCCredentials`, so one value covers unary and stream calls,
  and sends no header at all when its token is empty.
- The header is `authorization: Bearer`, the executor's shape, not the
  `x-fleet-provisioner-token` the reverse direction uses. Renaming that one
  would be its own two-sided rollout for no security gain, so it stays.
- **A second shared secret, not a reuse of `FLEET_PROVISIONER_TOKEN`.** That
  one authenticates the provisioner to core. Reusing it would let whichever
  side leaks impersonate the other in both directions. Two keys cost one more
  Infisical entry.
- **Wiring:** a new key, `FLEET_CORE_PROVISIONER_TOKEN`, in Infisical project
  `agent-fleet-nygh`, env `dev`. Core (`infisical.enabled` in `k8s/core.yaml`)
  and the provisioner (`k8s/provisioner/infisicalsecret.yaml` + `envFrom`) both
  already sync that whole scope, so no manifest changes. Worker pods do not get
  it: the provisioner builds their env from an explicit list
  (`provisioner/internal/k8s/pod.go`), never from `envFrom`.
- Core only warns when its copy is unset (as it does for
  `FLEET_PROVISIONER_TOKEN`): the console, Discord and the transcript keep
  working without a provisioner, and every provisioner call failing
  `Unauthenticated` is loud enough.

### 2. The executor fails closed (PR A)

- An empty `THOT_AUTH_TOKEN` makes `newGRPCServer` return an error and the
  executor exits at startup.
- The check runs on unary **and** stream calls. `ExecutorService` has no stream
  RPC today; the stream half is there for the same reason as §1's. There is no "dev only" open mode any more. The
  kind harness does not run the executor, so nothing loses a workflow.
- The bearer compare uses `crypto/subtle.ConstantTimeCompare`.
- Verified live before the change: `thot-executor-infisical` in `thot` carries
  a non-empty `THOT_AUTH_TOKEN`, so the next image pull starts normally.
- The `THOT_AUTH_TOKEN`-in-worker-env path for admin-only `cluster_access`
  sessions is unchanged. ADR-0072 (approval-gated executor) replaces it later.

### 3. NetworkPolicies in `agent-fleet` (later G0 PR)

- Default-deny **ingress** for the namespace, then explicit allows:
  - Traefik (`traefik` namespace) → core `:8080`. This also carries the
    Alertmanager webhook, which posts through the ingress.
  - Prometheus (`monitoring`) → core `:9093` and provisioner `:8080`
    (`/metrics`).
  - core → provisioner `:9090`.
  - session pods (sidecars) and the provisioner → core `:9090`.
  - Traefik → session pods (`agent-fleet.dev/component: worker`) on **any**
    port. `ExposeSession` puts a Service in front of the session pod on a port
    the agent picks, so the `<id>-e2e.bnei.dev` previews need this.
  - session pods → shared-instance pods
    (`agent-fleet.dev/component: shared-instance`, the Postgres/Redis
    `ProvisionService` creates) on their service ports.
- No foreign namespace reaches the provisioner or core's `:9090`. The token
  (§1) and the lease (ADR-0057) stay the authentication. The policies only
  shrink who can try.
- Egress is out of scope here. The session pod egress lock is ADR-0064.
- Acceptance is live, not just rendered: a pod in a foreign namespace fails to
  connect to both, and Traefik, Prometheus and the Alertmanager webhook keep
  working, an exposed session preview still loads through Traefik, and a
  session pod still reaches its shared Postgres and Redis.

### 4. Console CSP (later G0 PR)

- Core sends `Content-Security-Policy: frame-ancestors 'none'` on every
  response except `/embed/*`. ADR-0068 sets the `/embed/*` policy from its
  origin allowlist.
- `frame-ancestors` only. A full script/style CSP for the SPA is a separate
  piece of work and is not claimed here.

### 5. Per-request admin check (later G0 PR)

- Every `DashboardService` call re-checks that the cookie's identity is still
  in `platform-admins`. A failing check is `PermissionDenied`.
- Cookie `Identity` gains `sub` so the check (and ADR-0066's audience checks)
  key on a stable subject, not a display name.
- Test: a non-admin cookie gets `PermissionDenied` on every `DashboardService`
  method, enumerated from the proto descriptor so a new method cannot skip it.

## Rollout of §1 and §2

§1 ships as two PRs, because every merge to `main` is a release and one
image-bump commit rolls both Applications together.

1. **The secret first.** `FLEET_CORE_PROVISIONER_TOKEN` must exist in
   Infisical before the new provisioner image starts, or it refuses to start
   (by design). It was created on 2026-10-18 (32 random bytes, hex), and was
   already synced into the live `provisioner-infisical` and
   `agent-fleet-core-infisical` Secrets on 2026-10-18.
2. **PR A: core's client, plus §2.** Core sends the header; the old
   provisioner ignores unknown metadata, so this release changes nothing it
   can break. Merge, let it release, and confirm the new core pod is running
   before PR B merges.
3. **PR B: the provisioner interceptor and its fail-closed boot.** This is
   the order that has no window. Shipped in one release instead, the
   provisioner goes first: both Applications (`agent-fleet-core`,
   `platform-provisioner`) auto-sync from the same image-bump commit, core
   is slower (its PreSync migration hook runs first), and the provisioner
   has no readiness probe, so the old provisioner pod goes as soon as the new
   container starts. For that gap, tens of seconds, every core → provisioner
   call is `Unauthenticated`, so messages fail to warm.
4. **Never roll core back below PR A's release while the provisioner is on PR
   B or later.** That reopens the same failure with no end: every pod create,
   teardown, expose and DSN request fails until core moves forward again.
   Roll the provisioner back first, or both together.
5. The executor floats on `:latest` (infra-bootstrap `gitops/platform/thot/`),
   so it picks up §2 on its next restart. Its token is already present.

## Consequences

- An attacker on the pod network needs a credential for every fleet gRPC
  surface: a lease for `CoreService`, `FLEET_PROVISIONER_TOKEN` to impersonate
  the provisioner, `FLEET_CORE_PROVISIONER_TOKEN` to impersonate core, and
  `THOT_AUTH_TOKEN` for the executor.
- Core holds `FLEET_CORE_PROVISIONER_TOKEN`, so core compromised still means
  the provisioner is compromised. That is the hub-and-spoke shape (ADR-0020),
  not a regression.
- Two more ways to fail at startup: provisioner and executor both exit on an
  empty token. That is intended. "Crashlooping, saying why" beats "running,
  open".
- Rotating `FLEET_CORE_PROVISIONER_TOKEN` needs both pods restarted. The
  `envFrom` is read at start, and the two sides disagree until both have
  restarted.
