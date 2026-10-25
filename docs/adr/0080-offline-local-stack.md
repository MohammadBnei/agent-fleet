# ADR-0080 — The local stack runs offline: the real vault, a local forge

- **Status:** Proposed
- **Date:** 2026-10-25
- **Related:** [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (the vault and the egress lock, unchanged here),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md) (owner
  vaults), [ADR-0070](0070-headless-invoke.md) (headless sessions that
  ask), [ADR-0066](0066-agent-service.md) §3 (clients on :9091),
  infra-bootstrap ADR-0061 (Agent Vault), `/kind-local`.

## Context

`/kind-local` runs the real fleet (core, provisioner, real worker and
sidecar Jobs) on a disposable kind cluster. It has been broken since
ADR-0075: every warm needs Agent Vault, and the local stack has none.

The owner now wants the fleet to run **offline**: on a laptop, with no
GitHub and no cluster, against a local git forge that a game (mountain R11,
`ukubi-game`) provides. The forge serves smart-HTTP at
`http://<host>:7480/<owner>/<repo>.git`, takes anonymous pushes, and records
every push in `pushes.jsonl`. The game orders work through **definition
sessions**.

What the code needs, and what the local stack lacks:

- **Agent Vault.** Every pod gets a vault session token, the vault CA and
  the proxy env. The vault runs from its pinned image in a local container
  on SQLite (infra-bootstrap ADR-0061, verified that way); a definition
  vault is filled from files the provisioner reads. Only the repo pod's
  **owner** vault needs more: a GitHub App installation token
  (`reconcileOwnerLocked`), which a laptop with no GitHub cannot mint.
- **The egress lock.** A definition warm requires the `CiliumNetworkPolicy`
  `definition-egress-lock` to exist (`EgressLockPresent` reads the object;
  it enforces nothing itself). kind has no Cilium.
- **github.com URLs.** `definitionWarmable` requires the repo's URL to be the
  GitHub repo the definition's credentials name. Definitions, packs and
  fleet-shared are cloned from `FLEET_SHARED_REPO_URL` (GitHub by default).

## Decision

### 1. The real vault, nothing switched off

`/kind-local` runs Agent Vault from its pinned image, on SQLite, with the
provisioner as its only client, exactly as production does. Definition pods
take the full production path: a minted session token, the CA, the proxy,
revoke on teardown. Their credential files come from a local directory: the
model token for `api.anthropic.com`, and a placeholder for `github.com`,
which nothing uses (§2). No real secret enters a pod, and no code path is
skipped, so ADR-0065 and 0075 hold unchanged.

- **The egress lock** is applied as the real object from
  `k8s/provisioner/definition-egress.yaml`, with Cilium's CRD so it can
  exist. kind enforces nothing; the skill says so. Pods reach the forge
  directly.
- **Repo sessions are out of scope offline.** Their owner vault needs a
  GitHub App token. The game uses definition sessions only.

### 2. GitHub URLs stay; git rewrites them

The data keeps its production shape, `https://github.com/<owner>/<repo>.git`,
so every check that reads it passes unchanged. Git's own `insteadOf` sends
the traffic elsewhere:

- **The provisioner** gets the rewrites as `GIT_CONFIG_*` env in its
  manifest, no code:
  - `https://github.com/MohammadBnei/agent-fleet` → a **read-only bare
    mirror** on the workspace volume, outside every pod's mounts, that
    `/kind-local` writes from the branch under test. Definitions, packs and
    fleet-shared come from it. No pod can push to the source of its own
    authority.
  - `https://github.com/` → the forge, for the target repos' cache.
- **The worker** needs the forge rewrite too, since its own `git push`
  reads the clone's origin. The provisioner appends
  `FLEET_GIT_URL_REWRITE` (unset in production) to the worker's
  `GIT_CONFIG_*`, after the entries it already sets. This is the one change
  to fleet code.
- The forge is plain HTTP, so the vault proxy (`HTTPS_PROXY`) never sees
  it, and git sends no credentials. The forge must take anonymous pushes,
  under a host name that stays the same for the life of the stack;
  `EnsureRepoFetched` refuses a cache whose origin moved.

### 3. The local core is reachable by a client

`local/kind/30-core.yaml` gains:

- port 9091 on the container and the Service (AgentService for clients);
- `FLEET_CLIENT_TOKEN_GAME` from the local Secret;
- `MAX_DEFINITION_SESSIONS=3` and `MAX_EXTERNAL_SESSIONS=2`. The external
  pool must sit inside the definition pool (`checkCapacity`), and at 0 no
  client session warms.

**No bridge offline.** The bridge authenticates its clients as authentik
service accounts (infra ADR-0066), which need authentik. A client learns
that a session waits from `WaitForResult` and `Status` (ADR-0070 §4).
`announceSessions: true` stays in `clients/game.yaml`: inert here, live
wherever a bridge is.

### 4. Game definitions live in `local/kind/definitions/`

They are sim-only. Production reads every file in `definitions/` and builds
a vault for each valid one, so they stay out of it:

- `local/kind/definitions/` holds an abstract `game-base` (credentials,
  identity, `audience`, `git.mode: push`, `invokers: [game]`,
  `sidecarTools: [AskUserQuestion]`, `TodoWrite` in `tools.allow`), one
  `game-<repo>` per target (its repo and `branch: game/<repo>`), and
  `clients/game.yaml` (`actors.allow: [MohammadBnei]`,
  `announceSessions: true`, and the two-sided `definitions:` list);
- `repo_test.go` loads `definitions/` together with them, with the same
  loader;
- `/kind-local` copies them into `definitions/` in the mirror's commit, so
  the provisioner (which reads only `definitions/` and `packs/`) and core's
  sync see them, and writes each target's `repos` row.

Adding a target is one `game-<repo>` file, its line in `clients/game.yaml`,
and its row. Which repos get one is the owner's choice.

### 5. `/kind-local` is repaired, not forked

The skill and `local/kind/` take this setup; there is no second local
stack.

- The mirror's commit is the branch under test plus the game files, and
  `FLEET_DEFINITIONS_REF` and `FLEET_SHARED_BRANCH` both name it.
- The model token comes from a local file, not `infisical run`, so the
  stack needs no Infisical. Use a separate, revocable token for local runs.
- **What stays online:** `api.anthropic.com`, through the vault. Building
  images and pulling the kind and vault images need the network once. An
  agent's own dependency installs (npm, Go modules) need it too, unless
  their caches are warm.
- The "not yet updated for ADR-0075" notes go from the README and the skill.

## Alternatives considered

- **A vault-off mode** (this ADR's first drafts). It puts the real model
  token in every pod, skips the code ADR-0065 and 0075 changed most, and
  needs a production guard, which a whole-project Infisical sync could
  bypass. The real vault runs locally.
- **A second provisioner (`localprov`) that spawns processes.** Rejected
  before (R11 sync): a second `ProvisionerService` drifts from the one
  production runs.
- **Forge URLs in the `repos` table.** No rewrite needed, but every
  github.com check must then be relaxed for the local stack.
- **Definitions read from the forge.** Every pod can push to the forge, so
  any session could rewrite the definitions that bound it.
- **Game definitions in `definitions/` or in `ukubi-game`.** In
  `definitions/`, production would build their vaults for nothing. In
  `ukubi-game`, nothing can run the loader, an `internal` package.

## Consequences

- The game's definition sessions run offline, on the code paths production
  runs, with no real secret in a pod; `/kind-local` works again.
- One provisioner env var, unset in production, is the only fleet code.
- Repo sessions still need GitHub.
- The forge's `pushes.jsonl` is the offline record of who pushed what; the
  commit trailers stay labels.
