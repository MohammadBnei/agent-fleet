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
`http://<host>:7480/<owner>/<repo>.git`; fetches are anonymous, a push gets a
401 challenge and any credentials pass, and every push is recorded in
`pushes.jsonl`, with the push's user name as `who`. The game orders work through **definition
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
provisioner as its only client, as production does. Pods take the full
production path: a minted session token, the CA, the proxy,
revoke on teardown. No real secret enters a pod, so ADR-0065 and 0075 hold
unchanged.

- **Bootstrap.** A small local script in `local/kind/` registers the vault
  owner, sets the instance invite-only, creates the provisioner's member
  agent and writes its token file. No Infisical: the production bootstrap
  (`agent-vault-bootstrap.sh`) reads and writes Infisical, so it is not
  reused. The image pin is read at stack-up from infra-bootstrap's
  `gitops/platform/values/agent-vault/values.yaml`, so there is one pin.
- **Credentials.** The provisioner's credential files come from a local
  directory: the model token (`api.anthropic.com`, a separate revocable
  token for local runs) and one `GH_<VAULT>` placeholder per game
  definition, which nothing uses (§2).
- **The egress lock** is applied as the real object from
  `k8s/provisioner/definition-egress.yaml`, with Cilium's CRD so it can
  exist. kind enforces nothing; the skill says so.
- **Repo sessions: a local owner token** (the owner, 2026-10-25). A repo
  pod's owner vault maps `github.com` to a GitHub App installation token,
  which a laptop with no GitHub cannot mint. `FLEET_OWNER_TOKEN_FILE`, when
  set and no App is mounted, gives the owner vaults that file's token
  instead; the provisioner refuses it while an App is mounted. The token
  stays in the vault, as the App's does; offline it is a placeholder the
  forge accepts, and with network it can be a real token for GitHub.

### 2. GitHub URLs stay; git rewrites them

The `repos` rows keep their production shape,
`https://github.com/<owner>/<repo>.git`, so every check that reads them
passes unchanged.

- **Definitions and packs** come from a **read-only bare mirror** on the
  workspace volume, outside every pod's mounts: `FLEET_SHARED_REPO_URL` is
  its path, and `safe.directory` names it in the provisioner's
  `GIT_CONFIG_*` (the host writes it, so git's ownership guard would refuse
  it). No pod can push to the source of its own authority.
- **Target repos.** The provisioner's manifest rewrites
  `https://github.com/` to the forge (`GIT_CONFIG_*`, `insteadOf`), so its
  cache clones from the forge.
- **The worker** needs the same rewrite for its own `git push`, which reads
  the clone's origin. The provisioner appends `FLEET_GIT_URL_REWRITE` to
  every pod's `GIT_CONFIG_*`, with the session id as the URL's user name: the forge challenges a push (401), takes any credentials, and
  records the user name as `who`, so each push names its session. The
  provisioner refuses to start with the variable set while its vault
  address is the production default.
- The forge is plain HTTP, so the vault proxy never sees a push, and the
  vault's per-repo path scoping, which confines a push-mode pod to its repo
  in production, does not apply: the forge takes any repo and branch.
  Offline, `pushes.jsonl` is the record, not a gate.

### 3. The local core is reachable by a client

`local/kind/30-core.yaml` gains:

- port 9091 on the container and the Service (AgentService for clients);
- `FLEET_CLIENT_TOKEN_GAME` (at least 32 bytes) from the local Secret,
  named by `clients/game.yaml`'s `tokenRef`;
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
a vault for each valid one, so they stay out of it.

- An abstract `game-base` holds what children can share: `audience`,
  `invokers: [game]`, `sidecarTools: [AskUserQuestion]`, `tools` with
  `TodoWrite` in `allow`, `limits`. Each `game-<repo>` carries its own
  `credentials` (the vault is named after the definition) and its whole
  `git` block (`mode: push`, `branch: game/<repo>`, `identity`), since
  inheritance replaces top-level keys.
- `clients/game.yaml`: `tokenRef: FLEET_CLIENT_TOKEN_GAME`,
  `actors.allow: [MohammadBnei]`, `announceSessions: true`, and the
  two-sided `definitions:` list.
- `repo_test.go` also loads them, mapped onto `definitions/` paths, with the
  same loader.
- The first target is `ukubi-game` itself (the owner, 2026-10-25). Adding
  one is its `game-<repo>` file, its line in `clients/game.yaml`, its
  `GH_<VAULT>` placeholder and its `repos` row.

### 5. `/kind-local` is repaired, not forked

The skill and `local/kind/` take this setup; there is no second local
stack.

- **The mirror's commit** is the branch under test with `definitions/`
  replaced by the game files: its packs and fleet-shared are exercised;
  production's definitions are left out, since no vault here could serve
  them. `FLEET_DEFINITIONS_REF` and `FLEET_SHARED_BRANCH` name it.
- **What stays online:** `api.anthropic.com`, through the vault. Building
  images and pulling the kind and vault images need the network once.
  Definition sessions have no package registry anyway: their vault denies
  every host it does not map.
- The "not yet updated for ADR-0075" notes go from the README and the
  skill.

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
- Two provisioner env vars are the only fleet code: `FLEET_GIT_URL_REWRITE`,
  refused against the production vault, and `FLEET_OWNER_TOKEN_FILE`,
  refused while an App is mounted.
- Repo and definition sessions both run offline.
- Offline, nothing confines a pod's push to its repo and branch; the forge
  records every push.
- The forge's `pushes.jsonl` is the offline record of who pushed what; the
  commit trailers stay labels.
