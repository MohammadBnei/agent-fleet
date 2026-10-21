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
- **An empty token refuses to start**, and so does a blank one or one shorter
  than 32 bytes (the live value is 64 hex characters).
  `grpcserver.NewGRPCServer` returns an error and the provisioner exits before
  it listens, the same fail-closed
  startup ADR-0056 chose for OIDC, for the same reason. `main` builds its
  server with that function, and so do the tests, so the tested interceptor
  chain is the shipped one.
- The interceptor is method-agnostic: every method, including ones added
  later, needs the token. There is no exempt list to forget to extend. The
  test enumerates every `ProvisionerService` method from the generated
  descriptor. A rejected unary call is still access-logged and counted, and
  every rejection logs a warning naming its peer address, so a probe of the
  port can be attributed. A rejected stream call gets the warning only: the
  stream chain has no access log or metrics yet, as there is no stream RPC to
  count.
- Core's `provisionerclient.New(addr, token)` attaches the header with
  `grpc.WithPerRPCCredentials`, so one value covers unary and stream calls,
  and sends no header at all when its token is empty.
- The header is `authorization: Bearer`, the executor's shape, not the
  `x-fleet-provisioner-token` the reverse direction uses. Renaming that one
  would be its own two-sided rollout for no security gain, so it stays.
- **A second shared secret, not a reuse of `FLEET_PROVISIONER_TOKEN`.** That
  one authenticates the provisioner to core. Two keys mean a leaked header or
  log line exposes one direction only, and each key rotates on its own. They
  do **not** isolate the two sides from each other's compromise: both pods
  sync the whole Infisical scope, and a symmetric secret has to be held by its
  verifier as well as its sender, so either pod's env holds both keys. Only
  per-direction asymmetric credentials (mTLS, signed tokens) would give that,
  and this ADR does not claim it. Two keys cost one more Infisical entry.
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

- An empty, blank or under-32-byte `THOT_AUTH_TOKEN` makes `newGRPCServer`
  return an error and the executor exits at startup. The live value is 64
  characters.
- The check runs on unary **and** stream calls. `ExecutorService` has no stream
  RPC today; the stream half is there for the same reason as §1's. There is no "dev only" open mode any more. The
  kind harness does not run the executor, so nothing loses a workflow.
- The bearer compare uses `crypto/subtle.ConstantTimeCompare`.
- Verified live before the change: `thot-executor-infisical` in `thot` carries
  a non-empty `THOT_AUTH_TOKEN`, so the next image pull starts normally.
- The `THOT_AUTH_TOKEN`-in-worker-env path for admin-only `cluster_access`
  sessions is unchanged. ADR-0072 (approval-gated executor) replaces it later.

### 3. NetworkPolicies in `agent-fleet` (later G0 PR)

*Amended 2026-10-18 by the PR that ships it. The first draft said default-deny
for the whole namespace and had the Alertmanager webhook arrive through the
ingress; what shipped is narrower, and the webhook does not.*

- Ingress-only policies that **select core and the provisioner**, nothing
  else. Selecting a pod makes it default-deny for ingress; the allows are
  every caller found live on 2026-10-18:
  - core `:8080`: the `traefik` namespace, plus Alertmanager and Grafana in
    `monitoring`. Both post `/webhook/alertmanager` to the ClusterIP
    `agent-fleet-core.agent-fleet.svc.cluster.local:8080`, not through the
    ingress (infra-bootstrap `gitops/platform/values/{prometheus,grafana}/values.yaml`).
    Dropping the `monitoring` rule is a #230-style outage.
  - core `:9090`: session pods (`agent-fleet.dev/component: worker`) and the
    provisioner, same namespace only.
  - core `:9093` and provisioner `:8080` (`/metrics`): Prometheus.
  - provisioner `:9090`: core.
- No foreign namespace reaches the provisioner or core's `:9090`. The token
  (§1) and the lease (ADR-0057) stay the authentication. The policies only
  shrink who can try.
- A new caller needs its rule first. ADR-0072's `thot-executor` (namespace
  `thot`) calling core is one: it needs a namespace-AND-pod peer
  (`thot` / `app=thot-executor`) on the port it uses, merged before the
  executor code that dials core. Without it the call times out rather than
  erroring, and with Hubble metrics off nothing counts the drop.
- The `:8080` rule refuses a worker pod's *direct* connection, nothing more.
  Worker egress is open and `fleet.bnei.dev` is a public, unproxied route with
  no path constraint, so a worker going through it arrives as Traefik and is
  admitted. Traefik also runs with `allowCrossNamespace=true`, so an
  `IngressRoute` in any namespace can front core's Service. The OIDC gate and
  `ALERT_WEBHOOK_TOKEN` stay the only boundary on `:8080`.
- **Deferred: the rest of the namespace.** Session pods, shared instances
  (`agent-fleet.dev/component: shared-instance`, Postgres/Redis with their
  admin secrets) and the `expose-*` Services stay unselected, so any
  namespace can still reach them. Fencing them needs Traefik → session pods on
  any port (`ExposeSession` previews) and session pods → shared instances,
  each proved live with a preview and a DSN. It goes with the session-pod
  work of ADR-0064 (P3), not with G0, whose acceptance is the provisioner and
  `:9090`.
- Plain `NetworkPolicy`, not `CiliumNetworkPolicy`: under this cluster's
  Cilium chaining mode it is enforced for traffic by ClusterIP and by pod IP,
  and the same labels from another namespace are refused (proved live in a
  scratch namespace before shipping).
- **Residual: the host identity.** Under Cilium chaining a source with the
  host identity on the target's own node is always admitted. That keeps
  kubelet probes working, and it also admits any `hostNetwork` pod on that
  node, past all three fences, including core's `:9093`, which has no
  credential (ADR-0059 counts its `/metrics` as sensitive). No namespace
  enforces Pod Security Admission `baseline`, so today that is anyone who can
  create a pod with `nodeName` set to core's or the provisioner's node:
  cluster-admins, ArgoCD, and the in-cluster actions-runner, which can
  `create jobs.batch` in `vos` and `vos-dev` (infra-bootstrap ADR-0022;
  checked with `kubectl auth can-i` on 2026-10-18). Closing it is PSA
  `baseline` on the user-app namespaces, an infra-bootstrap change.
  The host identity probably also covers NodePort and LoadBalancer traffic:
  Cilium runs without kube-proxy replacement here, so kube-proxy (IPVS)
  masquerades it to the node IP, and a backend on the receiving node sees
  the host. A selectorless NodePort/LB Service plus a hand-written
  EndpointSlice at core's or the provisioner's pod IP, in any namespace,
  would then expose `:9090`/`:9093` to the LAN. **Not tested live**; inferred
  from the spike's KPR and IPVS facts. Who can create those objects is
  mostly the same population (cluster-admins, ArgoCD via app repos'
  `extraManifests`). Closing it is restricting `services` of type
  NodePort/LoadBalancer and `endpointslices`, or Cilium's BPF NodePort, which
  keeps the source identity.
- Egress is out of scope here. The session pod egress lock is ADR-0064.
- The selectors are guarded two ways. `provisioner/internal/k8s/networkpolicy_test.go`
  pins the in-repo labels and ports (`WorkerLabels`, `SharedInstanceLabels`,
  the provisioner's pod template and containerPorts, core's service ports and
  `CORE_*_PORT`) and that both policies are ingress only. Core's release name
  lives in infra-bootstrap's `gitops/apps/registry.yaml`, which this repo
  cannot read: the test copies it, and the provisioner policy's literal must
  change with it in the same rollout, or core loses `ProvisionerService`.
  Core's migrate hook pod carries the same selectorLabels and is admitted
  too, accepted (core's own image and credentials). The external callers'
  labels (Traefik, Prometheus, Alertmanager, Grafana, set by third-party
  charts in infra-bootstrap) are only asserted there; a relabel shows up as
  `TargetDown` for the core and provisioner ServiceMonitors and as
  `AlertmanagerFailedToSendAlerts` for Alertmanager's webhook, both live
  kube-prometheus-stack rules. Grafana's webhook has no such alert.
- Acceptance is live, not just rendered: a pod in a foreign namespace
  actually connects and fails, to both (not "no rejections in the logs"), and
  Traefik, Prometheus and the Alertmanager webhook keep working. Grafana's
  webhook has no alert, so it is checked by hand: send a test notification
  from Grafana's agent-fleet contact point and confirm a proposal lands in
  core.

### 4. Console CSP (later G0 PR)

- Core sends `Content-Security-Policy: frame-ancestors 'none'` on every
  response except `/embed/*`. ADR-0068 sets the `/embed/*` policy from its
  origin allowlist. *(Amended by [ADR-0068](0068-islands.md): there is no `/embed/*`;
  islands are web components in the host page, so `frame-ancestors 'none'`
  holds on every path, and each host must send `frame-ancestors 'self'`.)*
- `frame-ancestors` only. A full script/style CSP for the SPA is a separate
  piece of work and is not claimed here.

### 5. Per-request admin check (later G0 PR)

- Every `DashboardService` call re-checks that the cookie's identity is in
  `platform-admins`. A failing check is `PermissionDenied`.
- The groups come from the cookie, which was minted at login. So this check
  guards against login being widened later (ADR-0066 lets audience groups
  log in). It does **not** revoke an admin removed in authentik: they keep
  access until the cookie expires (up to 12h). Revocation is an open item, a
  shorter TTL or a refresh-time group re-read.
- Cookie `Identity` gains `sub`, so the check and ADR-0066's audience checks
  key on a stable subject, not a display name. The fleet provider's
  `sub_mode` is `hashed_user_id` (infra-bootstrap#342, 2026-10-18), so `sub`
  survives an email change. Earlier, `user_email` made it the address.
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
   is slower (its PreSync migration hook runs first), and the new provisioner
   is ready seconds after it starts. For that gap, tens of seconds, every
   core → provisioner call is `Unauthenticated`, so messages fail to warm.
   PR B is opened based on PR A's branch and retargeted to `main` before PR A
   is squash-merged (a squash merge of A would otherwise close B). PR B merges
   only once PR A's release run has finished and the core pod runs A's image
   tag, checked on the pod, not inferred from the merge: `docker.yml`'s
   release concurrency group cancels an in-flight release and releases from
   `main`'s tip, so merging B early folds both into one release.
4. **Never roll core back below PR A's release while the provisioner is on PR
   B or later.** That reopens the same failure with no end: every pod create,
   teardown, expose and DSN request fails until core moves forward again.
   Roll the provisioner back first, or both together.
5. The executor floats on `:latest` (infra-bootstrap `gitops/platform/thot/`)
   and nothing restarts it on release. After PR A releases, the operator runs
   `kubectl rollout restart deploy/thot-executor -n thot` and confirms the
   running pod's image digest is the new `:latest`. Until then the cluster
   runs the fail-open binary while CI is green. Its token is already present.
6. **Live negative check after PR B** (and after step 5 for the executor):
   from a throwaway pod in `agent-fleet` (once §3's fences are in, a foreign
   namespace is refused before auth is reached), `grpcurl` to
   `provisioner:9090` and `thot-executor.thot:9090` with no token and with a
   wrong token answers `Unauthenticated`, and a real session warm still
   succeeds. Zero rejections in the logs does not show the check works.
7. The provisioner has a readiness probe on `/healthz`, which only answers
   after the token check. A rollout with an empty or missing Secret stalls
   with the old pod serving instead of replacing it with a crashlooping one.
   The cost is that every rollout runs two reconcile loops for a probe period
   or two. That is accepted: their GC deletes ignore `NotFound`, and a repeated
   terminal report rewrites the same `sessions.pod_phase`.

## Consequences

- An attacker on the pod network needs a credential for every fleet gRPC
  surface: a lease for `CoreService`, `FLEET_PROVISIONER_TOKEN` to impersonate
  the provisioner, `FLEET_CORE_PROVISIONER_TOKEN` to impersonate core, and
  `THOT_AUTH_TOKEN` for the executor.
- Every key in the `agent-fleet-nygh`/`dev` scope reaches core, the
  provisioner and core's migrate PreSync hook Job (the chart gives hooks the
  same `envFrom`): all three sync the scope whole. That is 25 keys in the live
  `provisioner-infisical` (key names read 2026-10-18), among them all three
  fleet tokens (`FLEET_PROVISIONER_TOKEN`, `FLEET_CORE_PROVISIONER_TOKEN`,
  `THOT_AUTH_TOKEN`), the database credentials (`AGENTFLEET_DB_*`), the file
  store's S3 keys, the registry and Docker Hub credentials, `GH_TOKEN`,
  `CLAUDE_CODE_OAUTH_TOKEN` and `DISCORD_BOT_TOKEN`. `cluster_access` worker
  pods also get `THOT_AUTH_TOKEN`. So compromising core, the provisioner or
  the migrate image yields all of it, `ExecutorService` included, which holds
  cluster RBAC. Accepted for G0: the tokens stop a pod that holds none of
  them, which is the gap this ADR closes. Narrowing it (per-component
  Infisical folders, so each pod gets only the keys it reads: the DB
  credentials in core alone, the executor token in the provisioner only) is a
  follow-up with ADR-0072.
- Two more ways to fail at startup: provisioner and executor both exit on an
  empty, blank or short token. That is intended. "Crashlooping, saying why" beats "running,
  open".
- Rotating `FLEET_CORE_PROVISIONER_TOKEN` needs both pods restarted. The
  `envFrom` is read at start, and the two sides disagree until both have
  restarted.
