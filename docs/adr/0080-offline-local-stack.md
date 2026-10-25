# ADR-0080 — The local stack runs offline: no vault, no GitHub, a local forge

- **Status:** Proposed
- **Date:** 2026-10-25
- **Amends:** [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (a definition pod holds no real secret, and is refused without
  `vault_token_applied`) and
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §7 and §10
  (the model token comes from the vault; "no env-token break-glass"): both
  stop holding in this mode, which exists only where GitHub is unreachable
  by construction.
- **Related:** [ADR-0070](0070-headless-invoke.md) (headless sessions that
  ask), [ADR-0066](0066-agent-service.md) §3 (clients on :9091),
  `/kind-local`.

## Context

`/kind-local` runs the real fleet (core, provisioner, real worker and
sidecar Jobs) on a disposable kind cluster. It has been broken since
ADR-0075: every warm needs Agent Vault, and kind has none (the notes in
`local/kind/README.md` and the skill). There is deliberately no way to run
a pod without the vault.

The owner now wants the fleet to run **offline**: on a laptop, with no
GitHub and no cluster, against a local git forge that a game (mountain R11,
`ukubi-game`) provides. The model API stays online. The forge serves
smart-HTTP at `http://<host>:7480/<owner>/<repo>.git`, takes pushes, and
records every push in `pushes.jsonl`.

What ties the fleet to the cloud today, from the code:

- **The vault.** Every `CreateWorkerPod` mints a vault session token, mounts
  the vault CA and sets the proxy env. A definition warm also needs the
  egress lock, a Cilium CNP that kind does not have. Core refuses a
  definition pod without `vault_token_applied`.
- **github.com URLs.** The repo pod's owner vault (`repoOwner`) and the
  definition credential check in `definitionWarmable` require them. Both
  exist to bind a GitHub credential to the right repo.
- **Everything else is data or already optional.** Repo URLs come from the
  `repos` table; definitions, packs and fleet-shared are cloned from
  `FLEET_SHARED_REPO_URL`; the repo sync runs only with
  `FLEET_REPO_OVERRIDES`; the bridge is off without `BRIDGE_URL`.

## Decision

### 1. `FLEET_VAULT_DISABLED=1`: no vault means no GitHub

One rule carries the safety: **in this mode, GitHub does not exist.** A
pod here holds the real model token, which ADR-0065 and 0075 exist to
prevent. It is acceptable only where nothing it could reach is the owner's
real code, and the rule makes that structural rather than a promise about
where the flag is set:

- **The provisioner** (the flag's owner) refuses any create whose repo URL
  is on `github.com`, and refuses to start while `FLEET_SHARED_REPO_URL` is
  on `github.com` or an Agent Vault credential is mounted. A production
  provisioner given the flag, by Infisical or a typo, therefore starts on
  nothing or warms nothing, and says why.
- **What it skips:** the vault token, the CA mount, the proxy env, the
  egress-lock check, `ReconcileVaults` (a no-op), and `repoOwner`. The
  worker gets the model token from the provisioner's own
  `CLAUDE_CODE_OAUTH_TOKEN` and no `GH_TOKEN`.
- **What it keeps:** every other part of a definition pod's profile
  (ADR-0064): `LEASE_ID` removed, `FLEET_DEFINITION`, the git identity, the
  `GIT_*` and telemetry env, no service-account token, `ndots`, the skills
  and genome mounts. Today these share a function with the proxy env
  (`vaultWorkerEnv`); the build splits them.
- **The create says so.** It answers `vault_token_applied: false` and a new
  `vault_disabled: true`.
- **Core** takes the same flag and checks the two against each other on
  every pod, repo and definition alike: a create answering
  `vault_token_applied: false` is refused unless both sides are in this
  mode, and a core in this mode refuses a pod whose provisioner is not. In
  this mode core also refuses any repo on `github.com` and skips
  `definitionWarmable`'s credential-path match, which binds a GitHub
  credential this mode never mints.
- A buildguard test fails if a manifest under `k8s/` sets
  `FLEET_VAULT_DISABLED`. It is a tripwire for the reviewed path; the
  GitHub rule above is the guard.

### 2. The forge is data, not a mechanism

- `FLEET_SHARED_REPO_URL` points at the forge's copy of agent-fleet, so
  definitions, packs and fleet-shared come from it.
  `FLEET_DEFINITIONS_REF` names the ref, as it already does for a dev
  build.
- The `repos` rows name forge URLs. `/kind-local` writes them; the repo
  sync stays off.
- The pod's clone is local (`clone --shared` from the cache), and its
  origin is that URL, so the agent's `git push` reaches the forge. No URL
  rewriting (`insteadOf`) in any component.
- **The forge contract** the stack relies on: anonymous push (the worker has
  no credential helper and `GIT_TERMINAL_PROMPT=0`), and a host name that
  does not change for the life of the stack. `EnsureRepoFetched` refuses a
  cache whose origin moved; the skill pins the name in the cluster's DNS.
- Repo sessions push branches to the forge. "Every result = PR" has no PR
  offline; the branch is the result.

### 3. The local core is reachable by a client

`local/kind/30-core.yaml` gains:

- port 9091 on the container and the Service (AgentService for clients);
- `FLEET_CLIENT_TOKEN_GAME` from the local Secret, the token
  `clients/game.yaml` names;
- `MAX_DEFINITION_SESSIONS=3` and `MAX_EXTERNAL_SESSIONS=2`. The external
  pool must sit inside the definition pool (`checkCapacity`), and at 0 no
  client session warms.

**No bridge offline.** The bridge authenticates its clients as authentik
service accounts (infra ADR-0066), which need authentik. A client learns
that a session waits from `WaitForResult` and `Status` (ADR-0070 §4).
`announceSessions: true` stays in `clients/game.yaml`: inert here, live
wherever a bridge is.

### 4. Game definitions live in agent-fleet, outside `definitions/`

`game-<repo>` definitions and `clients/game.yaml` are sim-only. Production
reads every file in `definitions/` and builds a vault for each valid one,
so they do not go there. They go to `local/kind/definitions/`:

- reviewed here, and validated by `repo_test.go` with the same loader, which
  is an `internal` package no other repo can import;
- each carries what every launchable definition must: an
  `api.anthropic.com` credential and, for `git.mode: push`, a `github.com`
  one naming its repo. Here they are inert (§1);
- `/kind-local` commits them on a `game` ref of the forge's agent-fleet
  copy, **on top of the branch under test**, so that branch's own
  definition and pack changes are exercised too. The provisioner reads
  `FLEET_DEFINITIONS_REF=game`.

Their shape: `git.mode: push`, `branch: game/<repo>`, `invokers: [game]`,
`sidecarTools: [AskUserQuestion]`, `TodoWrite` in `tools.allow`;
`clients/game.yaml` with `actors.allow: [MohammadBnei]` and
`announceSessions: true`. Which repos get one is the owner's choice; the
list is files, with no code tied to a repo.

### 5. `/kind-local` is repaired, not forked

The skill and `local/kind/` take this mode; there is no second local stack.

- The model token comes from a local file, not `infisical run`, so the
  stack starts with no Infisical.
- Building the images and pulling the kind node image need the network
  once; after that the stack runs with only the model API.
- The "not yet updated for ADR-0075" notes go from the README and the
  skill.

## Alternatives considered

- **Bind the mode to `FLEET_AUTH_DISABLED=1`.** That flag is production's
  documented break-glass for an authentik outage, so it says nothing about
  being on a laptop. "No GitHub" does.
- **A local Agent Vault.** It keeps the pod shape identical, but it needs
  the vault's image, storage, bootstrap and a Cilium CNP in kind, to guard
  the owner's own token on the owner's own laptop.
- **A second provisioner (`localprov`) that spawns processes.** Rejected
  before (R11 sync): a second `ProvisionerService` drifts from the one
  production runs, and the point of the local stack is to run that one.
- **`insteadOf` rewriting to the forge.** It works for the provisioner by
  env, but the agent's own push needs the worker's env to carry it, which is
  code; forge URLs in the data need none.
- **Game definitions in `definitions/` or in `ukubi-game`.** In
  `definitions/`, production would build their vaults, with push
  credentials, for nothing. In `ukubi-game`, nothing can run the loader on
  them.

## Consequences

- The fleet runs on a laptop with the model API as its only outside
  dependency, and `/kind-local` works again.
- The mode cannot touch GitHub, so a misplaced flag fails loudly instead of
  running real repos with a real token in the pod.
- One flag on two components, cross-checked on every pod.
- The forge's `pushes.jsonl` is the offline record of who pushed what; the
  commit trailers stay labels.
