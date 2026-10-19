# ADR-0065 — Agent Vault holds definition credentials; definition pods hold placeholders and an egress lock

- **Status:** Accepted
- **Date:** 2026-10-19
- **Related:** [ADR-0064](0064-definition-pod-profile.md) (amended: its INTERIM
  model token, "cannot push" for `git.mode: push`, the open-egress consequence
  and the URL-binding deferral all close here),
  [ADR-0062](0062-defined-agents.md) §7 (amended: `limits.timeout` is no longer
  inert; it is the session's idle timeout, amended 2026-10-19), [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md)
  (the provisioner also holds the vault agent token; core holds none),
  [ADR-0004](0004-subscription-oauth-not-metered-api.md) (still the
  subscription OAuth token, now injected by the vault instead of held by the
  pod), the locked "every result = PR" invariant (amended for push mode), [ADR-0069](0069-component-auth-and-network-fences.md)
  (the ingress fences this adds an egress one to). The vault itself is
  infra-bootstrap ADR-0061 ("Agent Vault as a private platform service").

## Context

After ADR-0064 a definition pod is narrower than a repo pod, but its worker
still holds the fleet's real `CLAUDE_CODE_OAUTH_TOKEN` (INTERIM), it cannot
push, and its egress is the namespace's: the internet, the LAN, node ports. The
plan's principle 1 is that a definition pod holds **no real secret**: every
outside credential is injected on the wire, per host and path, by a broker the
pod cannot read. Infisical Agent Vault v0.40.0 is that broker (spikes 2 and 3):
a MITM proxy keyed by a short-lived session token (`av_sess_`) that overwrites
`Authorization` for the hosts and path prefixes a vault lists, and with
`unmatched_host_policy=deny` refuses everything else. Spike 1 showed a Cilium
`toEndpoints`+`toPorts` policy is judged on the backend identity after the
ClusterIP DNAT under chaining mode, so a pod can be fenced to exactly the vault
proxy, core's `CoreService` and DNS for those two names.

## Decision

### 1. The provisioner is the vault's only client, and holds its only credential

The provisioner gets a member-role Agent Vault agent token (`av_agt_`, no
expiry) in Infisical project `agent-vault` folder `/agent-vault`, minted by infra-bootstrap's
`gitops/scripts/agent-vault-bootstrap.sh` (ADR-0061). A new InfisicalSecret
(`k8s/provisioner/infisicalsecret-agent-vault.yaml`) syncs that folder only into
Secret `provisioner-agent-vault`, mounted read-only at `/var/run/agent-vault`
(`optional: true`: the provisioner still starts before the bootstrap ran). It is
a file per key, read per call, not `envFrom`: kubelet refreshes it, so a
rotation needs no restart, and the values never enter a child process's
environment (git, gh). The same folder holds the credential **values** the
definitions name (`CLAUDE_CODE_OAUTH_TOKEN`, as an Infisical reference to the
root key; `GH_DEV_NOTES`). Core never syncs that folder (its scope is the
non-recursive root) and never sees a value: it sends key **names**.

This is a deliberate deviation from the plan's "reuse the provisioner's
existing InfisicalSecret (`k8s/provisioner/infisicalsecret.yaml`)": a second
InfisicalSecret, one folder, a file mount and the `${…}` reference check
instead of more `envFrom` keys. Reasons: a definition can then only name keys
in `/agent-vault` (never the provisioner's own `GH_TOKEN`, DB or core
credentials, which `envFrom` puts in the same environment), a rotation reaches
the running provisioner without a restart, and no credential value enters a
child process's environment.

Keys starting `AGENT_VAULT_` or `FLEET_VAULT_` are refused by the validator and
again by the provisioner (`FLEET_VAULT_` is the provisioner's own synthetic-key
prefix), so a definition can never upload the agent token into a vault. A value
matching `^\$\{.*\}$` is refused as an unexpanded Infisical reference. Errors
name the key, never the value.

`provisioner/internal/vault` is one client, no interface. It does not trust
core's spec: before any write it re-checks the vault slug (never `executor-`,
the bridge executor's reserved prefix), the host allowlist
(`github.com`, `api.github.com`, `api.anthropic.com`), the path shape (the
validator's `githubPath`/`apiPath` regexes and dot-segment rule, ported),
"`api.anthropic.com` takes no paths", one `<owner>/<repo>` per vault, and the
**key binding**: a GitHub service's key must be `GH_` + the vault name
upper-snaked (`dev-notes` → `GH_DEV_NOTES`, `vault.GitHubKey`), and
`api.anthropic.com` never takes a `GH_` key. `CreateWorkerPod` checks
`vault.name == definition name`. A buggy or compromised core cannot point a
credential at another host, give one vault another definition's PAT, or send
the model token to GitHub. What core can still choose is which repo of the
token's owner a vault's paths name; for `dev-notes` nothing on GitHub's side
bounds that, because `GH_DEV_NOTES` is the bot's broad token (accepted risk,
see Consequences). Upgrade: the provisioner parses the definitions from
its own git read at the pinned ref and treats core's spec as a selector only.

### 2. Reconcile: the vault matches git

Desired state is every valid HEAD definition's `credentials`. Core pushes the
full set with `ProvisionerService.ReconcileVaults` after every successful
definitions sync (60s), and the provisioner, for each vault: creates it if
absent (409 = success), sets each credential whose value is new or changed
(an in-process sha256 memo, so an unchanged tick writes nothing; the memo is
empty after a provisioner restart, so the first reconcile rewrites every
credential, and a value changed in the vault itself, a restored PVC or a UI
edit, is not detected until then), `PUT`s the service set only
when it differs (a `PUT` replaces, so a service removed from git is removed),
sets `unmatched_host_policy=deny`, then deletes credentials no service uses.
A **widened** service set (any service not already there) first revokes every
session on the vault: vaults are per definition, not per pinned version, so
without it a release that adds a push service would hand it, within 60s, to
live pods warmed under the older pr-mode pin. The provisioner then deletes that
definition's live Jobs: on a dead token every proxied call gets 407 until the
pod's lifetime cap, and core's `WarmIfIdle` never re-warms a live phase. Gone,
core's pod-phase reconcile marks them TERMINATED (≤60s) and the next message
re-warms at the new pin. A narrowing keeps them. Core runs `ReconcileVaults`
off the sessions loop (its own goroutine, a tick still running skips the next),
so a hung vault never delays the ordinary-session sweeps.
Then it **prunes**: every vault it administers that is absent from the request
gets an empty service set and no credentials (vault kept; prefix `executor-` is
reserved for the event bridge's executor, ADR 0072, and skipped). An invalid or
deleted definition is absent, so its vault is emptied: the same fail-closed rule
as `Registry.Resolve`. A prune failure is `ReconcileVaultsResponse.prune_error`,
never a fake per-vault result. A second identical call is three GETs and no
write: unit-tested on the fake, and checked against a real v0.40.0 container
(`vault_live_test.go`, `-tags=integration`): its GET rejoins `host`+`path`
(`broker.Service.MarshalJSON`), so the client's inline `host` round-trips.

A service whose secret is missing is **dropped** from the desired set (a value
removed from Infisical revokes the vault's copy) and the reconcile errors
naming the key. The same reconcile runs inline in `CreateWorkerPod` for the one
definition being warmed (never prune there), so a warm never depends on
in-memory state, and a missing secret refuses the warm with its name, before any
pod exists. A per-vault mutex serialises the periodic and the inline call.

Rendering: every host is pinned to `:443` (`api.anthropic.com:443`,
`github.com:443/<o>/<r>/*`): a service without a port matches every port, and
the vault's plain `http://` forward path injects too, so an unpinned
`http://github.com/...` would carry the PAT in cleartext. `api.anthropic.com` →
one `bearer` service (Claude Code's `Authorization: Bearer <placeholder>` is
overwritten whole). `github.com` paths → one service per path, `basic` with
username key `FLEET_VAULT_GIT_USERNAME` (= `x-access-token`, set by the
provisioner) and password = the definition's key. Basic was picked by a wire
check, not by convention: with one valid token, github.com's
`info/refs?service=git-receive-pack` on a private repo answered **200 to Basic
and 401 to Bearer**. Because the vault injects on the first request, git never
sees a 401 and asks no credential helper. `api.github.com` paths → one
`bearer` service per path.

**Checked end to end (2026-10-19)**, a local v0.40.0 container (same digest)
reconciled by this client, the exact `definitionWorkerEnv` rows, the worker
image with Claude Code 2.1.301 (amd64): `git ls-remote` of the private
`MohammadBnei/notes` exits 0; its receive-pack advertisement is 200; another
repo on github.com is 403 `no_match`; `api.github.com/repos/<o>/<r>/commits` is
200; one real `claude -p` with the real setup-token injected answers (no error,
`/v1/messages` 200 in the vault log), and an HTTP MCP server on `127.0.0.1`
was reached directly (NO_PROXY honoured by the native binary under the full
proxy env); `http://` on port 80 to a matched host is 403 `no_match`. The
GitHub token was a stand-in (the operator's `gh` OAuth token, repo scope),
not `GH_DEV_NOTES` (the bot token). Not done: an actual `git push`
(receive-pack POST) and its time against the vault's 60s `ReadTimeout`
(rollout step 6). Note `/repos/<o>/<r>` itself (`gh repo view`) is outside
`/repos/<o>/<r>/*` and gets 403.

### 3. One token per warm, revoked on every path

`Mint(vault, label, ttl)` / `Revoke(vault, label)` are generic: the event
bridge's executor will reuse them (one vault per executor scope, label = the
approval step). Label = `<sessionID>.<unix-ms>`, kept on the Job as annotation
`fleet.bnei.dev/vault-label` (not a secret). The mint response carries no id,
so `Revoke` lists the vault's sessions and deletes the one whose label is
**exactly** that label (a prefix would take `exec-10` with `exec-1`). Only a
Job labelled `fleet.bnei.dev/definition` is revoked, in that one vault: a repo
session never calls the vault, and an exact label never hits a re-warm's new
token, even when a provisioner restart re-sights a retained Failed Job.
Revokes are best-effort with their own 3s budget, detached from the caller's
cancel; the TTL bounds a miss.

| Path | Revoke |
|---|---|
| Job create fails after the mint | inline, exact label |
| Reconcile / CA / mint fails | nothing was minted |
| core refuses the answer (`vault_token_applied` absent, or an error) | `refuseDefinitionPod` → `TearDownSession` |
| Archive, Delete, rollback, stop-grace, startup-stall, idle | `TearDownSession` → `Revoke(definition, label)` from the Job, before the Job delete |
| Stop, worker crash, lifetime cap | `gcTerminalWorkerJobs`, `Revoke(definition, label)` before its `ReportEvent`, retried every tick until it lands while the Job is listed (a kept Failed Job holds the token in its pod env) |
| Job already gone at teardown | revoked when it went terminal, else TTL |
| provisioner dies mid-warm, revoke times out | TTL |
| open keep-alive tunnel after a revoke | ~2 min vault `IdleTimeout` (accepted) |
| definition narrowed / invalid / deleted | next `ReconcileVaults` (≤60s): narrowed or emptied, live tokens kept |
| definition widened (or a missing secret restored) | that reconcile revokes every session on the vault before the `PUT`, then deletes the definition's live Jobs |

**`limits.timeout` is a definition session's idle timeout** (removed from
ADR-0062 §7's inert list). *Amended 2026-10-19:* it first shipped (4.22.0) as
the pod's wall-clock lifetime, which is wrong: a timeout is "no interaction for
this long". Core's idle sweep (`ListIdleSessionIDs`) uses the pinned version's
`limits.timeout` in place of the global `IDLE_TIMEOUT_MS` for a definition
session; a repo session keeps the global one. Separately, a definition pod has
a fixed **12h lifetime cap** (provisioner `definitionPodLifetime`), so its token
always expires: Job `activeDeadlineSeconds = 12h`; token TTL = that + 60s
(`activeDeadlineSeconds` counts from the Job's `startTime`, set at create, so
the slack covers ms of skew): the pod always dies before its token. A Job
failed with reason `DeadlineExceeded` reports **TERMINATED** "pod lifetime cap
reached" and is deleted like a Succeeded one, so it shows idle, not failing,
and the next message re-warms with a fresh token and resumes. The cap can end
a turn in flight (accepted). `DefinitionPodProfile.timeout_seconds` is still
sent (an older provisioner requires it) but no longer read.

**One lock per vault orders warms against widenings.** A warm holds the
vault's lock from its reconcile through the mint to the Job create; a widening
reconcile holds it through revoke-all, the `PUT` and deleting the definition's
live Jobs. So a widening lands wholly before a warm (whose own reconcile then
sees the wide vault) or wholly after (its revoke and Job delete then reach that
pod): never a pod on an older, narrower pin holding a token on the wider vault,
nor a pod created after the listing on a token already revoked. Cost: warms
of one definition, and the core tick's reconcile of it, run one at a time (a
warm is seconds). Widening is judged on (host, auth) pairs, not service names:
names are positional (`github-0`, ...), so dropping or reordering a path is a
narrowing and keeps live tokens. `TestVaultLock_SpansWarmAndWidening` and
`TestReconcile_WideningRevokesSessions` (revoke before `PUT`) pin both.

### 4. The pod holds placeholders

Definition worker env: `CLAUDE_CODE_OAUTH_TOKEN` and `GH_TOKEN` =
`fleet-vault-placeholder`; `HTTPS_PROXY`/`https_proxy` =
`http://<av_sess_>:<vault>@agent-vault.agent-vault.svc.cluster.local:14322`,
and deliberately **no** `HTTP_PROXY` (the vault injects on plain http too: a
plain-http request goes direct and the egress lock drops it), and no
`.cluster.local` in `NO_PROXY` (such a request goes to the proxy, whose
egress denies private ranges, and would be dropped direct anyway). Both depart
from the P3 contract's env list (which named `HTTP_PROXY` and `cluster.local`)
on purpose: each keeps a request on the path with two refusals instead of one;
`NODE_USE_ENV_PROXY=1` (Node's built-in fetch ignores the proxy vars without
it; Bun and Claude Code honour them anyway, spike 2);
`NO_PROXY`/`no_proxy` = loopback (the sidecar), core and the vault API;
`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`, `GIT_SSL_CAINFO`,
`CURL_CA_BUNDLE` = `/etc/agent-vault/ca.pem`; `GIT_TERMINAL_PROMPT=0`;
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` (telemetry the vault would deny).
`definitionWorkerEnv` builds these rows; the bridge executor extracts them when
it needs them. The CA comes from the provisioner (`GET /v1/mitm/ca.pem`)
into ConfigMap `agent-vault-ca`, upserted on change and mounted as a required
volume, so the worker fetches nothing and a pod never starts without it. The
pod sets `automountServiceAccountToken: false` (the namespace's `default` SA
would otherwise get an API-server token mounted at admission, readable by the
agent's `Read` tool) and `enableServiceLinks: false` (no Service env vars). The sidecar gets no proxy env (grpc-go honours `HTTPS_PROXY`; it dials core
directly) and the clone init container needs none (local transport only). Pod
`dnsConfig` `ndots:1` (REFUSED + `ndots:5` breaks musl, spike 1 (b)). The worker
skips `gh api user` when `FLEET_DEFINITION` is set: the placeholder `GH_TOKEN`
reaches nothing outside `/repos/<o>/<r>/*`.

After P3 no container of a definition pod, init containers included, holds a
real secret: `TestDefinitionPod_NoRealSecret` asserts it with sentinel values in
the provisioner's environment, plus both pod-level flags above (the SA token is
admission-mounted, so the template is the only place a test can see it). The
pod does hold two per-session credentials, both minted for it: the worker's
`av_sess_` (accepted risks) and the sidecar's `LEASE_ID` (the CoreService
credential, ADR-0057); the test excludes both by name. A repo pod's Job is byte-identical
(`TestCreateWorkerPod_RepoSessionGolden`).

`CreateWorkerPodResponse.vault_token_applied` is set only by a provisioner that
minted the pod's token and built it without real credentials; a definition warm
whose answer lacks it got a P1 pod, and core refuses it exactly like a missing
`definition_profile_applied` (lease cleared, pod torn down).

### 5. The egress lock

`k8s/provisioner/definition-egress.yaml`, a `CiliumNetworkPolicy` selecting
`fleet.bnei.dev/definition` (any value), allows egress to: the vault pods
(`agent-vault` namespace) on `:14322` only; core's pods on `:9090`
(`CoreService`, the sidecar) only; the host entity on `:53` with an L7 DNS rule
whose `matchName`s are the vault's and core's FQDNs. Nothing else: no internet,
LAN, vault API, node ports (`:9090`, `:10250`), Prometheus or other core ports.
The matchName list also closes DNS tunnelling. A test pins the selector to
`k8s.DefinitionLabel`, core's labels to the provisioner's ingress policy, and
the names to the config defaults (both FQDNs, absolute first under `ndots:1`).
A warm checks the CNP exists (`get` on that one name, Role) before it mints and
refuses with `FailedPrecondition` otherwise: a failed sync, a delete before
selfHeal or a cluster without Cilium would else give the pod its token with
open egress. Presence, not enforcement: that was verified live (spike 1).

What breaks in a definition pod, deliberately: `WebFetch` and any HTTP outside
the vault's rules (403 `no_match`); `npm`/`bun`/`pip`/`go`/`cargo` installs;
`gh` outside `/repos/<o>/<r>/*`; Playwright/Chromium browsing (Chromium ignores
`HTTPS_PROXY`, the CNP drops it direct); submodules and LFS objects on other
hosts; Claude Code auto-update and telemetry; any MCP server that needs the
network. What works: Claude Code to `api.anthropic.com` (every path), server-side
`WebSearch`, git fetch/pull/push to the one repo, local tools, the sidecar to
core. The repo cache, claude-home, browsers and `/cache` are volumes, not network.

### 6. URL binding (ADR-0064's deferral)

Before a pod has a push credential, the repo it clones from and pushes to is
bound to the reviewed definition:

1. **Core, at warm**, before `ReserveSlot`: when the effective spec has a
   `github.com` service, `repos.url` must be exactly
   `https://github.com/<o>/<r>` or `…/<o>/<r>.git` for the one `<o>/<r>` its
   paths name (the validator requires one pair). Else FailedPrecondition, no
   lease, no phase.
2. **Provisioner, cache**: `EnsureRepoFetched` refuses a cache whose
   `remote.origin.url` is not the request's `repo_url` (modulo `.git`): a
   repointed URL never leaves a warm cache cloned from somewhere else.
3. **Vault**: credentials are injected only on `github.com/<o>/<r>/*` and
   `.git/*`; under deny any other remote is a 403.

The branch is not bindable at the vault or a fine-grained PAT: `git.branch` binds
through the tool surface (P4 `canUseTool`).

### 7. Validator additions (the ceiling tightens; re-judge revokes old rows)

A non-abstract definition needs `credentials` with an `api.anthropic.com`
service; `credentials.vault` is ≥3 characters and does not start `executor-`;
`secret` does not start `AGENT_VAULT_`/`FLEET_VAULT_`; a GitHub service's
`secret` is `GH_` + the vault name upper-snaked and `api.anthropic.com`'s is not
a `GH_` key; `github.com`/`api.github.com`
services only with `git.mode: push` (pr mode keeps no push credential), and push
needs a `github.com` service; all of a definition's GitHub paths name one
`<owner>/<repo>`.

### 8. Compatibility

| State | Effect |
|---|---|
| this provisioner, vault not deployed / not bootstrapped | reconcile errors logged; every definition warm FailedPrecondition naming the cause; repo sessions unchanged |
| new core, older provisioner | `vault_token_applied` absent → refused, torn down; `ReconcileVaults` Unimplemented → debug log |
| older core, this provisioner | the profile lacks the vault → InvalidArgument → the warm fails |
| merge before the image bump | `k8s/provisioner` tracks `main`, so the CNP lands while 4.21.0 runs: a P1 definition pod would lose Anthropic. None is live (no definition repo is onboarded) |
| image rollback to 4.21.0 | P1 pods again, but the CNP stays: they cannot reach Anthropic. Fails closed |

## Accepted risks

- `/agent-vault` lives in the dedicated project `agent-vault`, read by its own
  identity `agent-vault-operator` (infra-bootstrap ADR-0061 Decision 6, moved
  2026-10-19 from `agent-fleet-nygh`). Residual: its credentials Secret
  `infisical/agent-vault-universal-auth` can be named by any InfisicalSecret CR,
  and that one identity also reads the vault server's folder. That reaches
  `AGENT_VAULT_TOKEN`, which can reveal every credential of every vault the
  provisioner administers.
- A leaked `av_sess_` works from another definition pod (same CNP) until its TTL.
- The vault injects on its plain `http://` forward path for a matching
  host:port. The `:443` pin makes port 80 a 403, but `http://api.anthropic.com:443/…`
  sent explicitly through the proxy still matches, and the vault sends the real
  credential in cleartext to the upstream's TLS port (checked with a dummy
  value: injected, upstream 400). Nothing in the pod does that by default (no
  `HTTP_PROXY`, https remotes, `curl` is an ask rule); it needs a command a
  human approves. Upgrade: an upstream option to refuse injection on the forward
  path, or an executor-style broker.
- The URL binding at the vault is a path **prefix** match on the decoded,
  uncleaned path (spike 3): `/<o>/notes/../../<other>/info/refs` matched the
  notes rule and got the credential. It does not reach another repo because
  github.com does not resolve dot-segments. Checked 2026-10-19 with the bot
  token on `info/refs?service=git-receive-pack` and `api.github.com/repos/...`:
  `..`, `%2e%2e`, `%2E%2E`, `..%2f`, `//..` and `notes.git/../..` all return
  404, while the plain other-repo path returns 200.
- **`GH_DEV_NOTES` is the provisioner bot's broad token, not a fine-grained
  PAT** (operator decision, 2026-10-19). So for `dev-notes` the vault's path
  rules plus GitHub's refusal of dot-segments are the only repo boundary. A bug
  in either, or an `api.github.com` rule wider than `/repos/<o>/<r>/*`, exposes
  every repo the bot can write. The token also sits in the vault's store, so a
  vault compromise yields the broad token. Upgrade: a fine-grained PAT on the
  one repo in place of the copy. Opus (P6) still requires a fine-grained PAT.
- The `av_sess_` is plaintext env (`HTTPS_PROXY` and its twin) in the Job and
  Pod spec: it sits in etcd, shows in `kubectl get -o yaml`/`describe`, and the
  agent can read `/proc/self/environ` with `Read`, so it can land in a
  transcript in core's DB. Bounded by its vault's rules and its TTL (up to
  the 12h lifetime cap + 60s). Upgrade: deny `Read(/proc/**)` in the
  definition tool deny list, or a shorter TTL re-minted on re-warm.
- A widening ends every live pod of the definition mid-turn (revoked, Job
  deleted); the next message re-warms and resumes.
- No real push has crossed the vault: fetch and the receive-pack
  advertisement have (§2), the push POST is rollout step 6.
- ~2 min of a revoked token on an open tunnel; the vault's 60s per-request
  `ReadTimeout` caps a single push upload (notes pushes are small; unmeasured,
  rollout step 6).
- The vault API is plain HTTP inside the cluster, fenced by ADR-0061's
  NetworkPolicy; credential values cross it on set.
- The 12h lifetime cap can end a turn in flight.
- An invalid HEAD empties the vault of live pods pinned to an older valid sha.
- No branch binding (force-push to `git.branch` or another branch of the repo).
- Agent Vault is a research-preview upstream: pinned by digest; the fallback is
  an executor-style broker (agent-fleet ADR-0037 shape).

## Rollout (operator)

1. Merge infra-bootstrap's ADR-0061 PR; ArgoCD syncs `agent-vault`. Run
   `gitops/scripts/agent-vault-bootstrap.sh` there once.
2. Store `GH_DEV_NOTES` in Infisical project `agent-vault`, env `dev`, path
   `/agent-vault`. For `dev-notes` it is a copy of the bot's `GH_TOKEN`, done
   2026-10-19 (accepted risk, see Consequences). A fine-grained PAT on the one
   repo is the stricter option and is required for Opus (P6).
3. Give the provisioner's bot read access to the private notes repo (the cache
   fetch uses the bot, not the vault).
4. Onboard repo `notes` in the console: url `https://github.com/MohammadBnei/notes.git`
   exactly (§6), base branch `main`, no image, cluster access off.
5. Merge this; the release brings definitions at the new tag.
6. **First push check:** from the first dev-notes session (or a `dev-*` pod
   behind the vault), one small commit pushed with the real `GH_DEV_NOTES`.
   Fetch, auth shape (Basic) and the receive-pack advertisement are already
   checked (§2); this proves the POST. Note the push
   time against the vault's 60s `ReadTimeout`.

## Consequences

- ADR-0064's INTERIM line is gone; "every result = PR" now reads: definition
  sessions in `git.mode: pr` cannot push (P4 opens PRs through the tool surface);
  `git.mode: push` sessions push to `git.branch` of their one repo.
- Outside credentials still live only in the provisioner, now as the vault's
  administrator, not in pods.
- `ProvisionerService` gains `ReconcileVaults`; `DefinitionPodProfile` gains
  `vault` and `timeout_seconds`; `CreateWorkerPodResponse` gains
  `vault_token_applied`; `ReconcileVaultsResponse` has `prune_error`. Additions only (`buf breaking` clean). No migration.
- The provisioner's Role gains `configmaps` create, and get/update on
  `agent-vault-ca` only (the CA).
