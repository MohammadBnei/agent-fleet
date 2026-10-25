# ADR-0080 — The local stack runs offline: no vault, a local forge

- **Status:** Proposed
- **Date:** 2026-10-25
- **Amends:** [ADR-0075](0075-github-app-identity-and-vault-credentials.md) §10 ("no
  env-token break-glass": one exists now, for the local stack only)
- **Related:** [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (the vault and the egress lock), [ADR-0070](0070-headless-invoke.md)
  (headless sessions that ask), [ADR-0066](0066-agent-service.md) §3
  (clients on :9091), [ADR-0056](0056-the-console-gate-is-oidc-in-core.md)
  (`FLEET_AUTH_DISABLED`, the pattern this follows), `/kind-local`.

## Context

`/kind-local` runs the real fleet (core, provisioner, real worker and
sidecar Jobs) on a disposable kind cluster. It has been broken since
ADR-0075: every warm needs Agent Vault, and kind has none
(`local/kind/README.md`, ADR-0075's own consequences). There is
deliberately no way to run a pod without the vault.

The owner now wants the fleet to run **offline**: on a laptop, with no
GitHub and no cluster, against a local git forge that a game (mountain R11,
`ukubi-game`) provides. Only the model API stays online. The forge serves
smart-HTTP at `http://<addr>:7480/<owner>/<repo>.git`, accepts any
credentials and push, and records every push in `pushes.jsonl`.

What ties the fleet to the cloud today, from the code:

- **The vault.** Every `CreateWorkerPod` mints a vault session token, mounts
  the vault CA and sets the proxy env. A definition warm also needs the
  egress lock (a Cilium CNP kind does not have). Core refuses a definition
  pod without `vault_token_applied`.
- **github.com URLs.** Two checks require them: the repo pod's owner vault
  (`repoOwner`) and the definition's credential path (`definitionWarmable`).
  Both are vault checks.
- **Everything else is data or already optional.** Repo URLs come from the
  `repos` table; definitions and fleet-shared are cloned from
  `FLEET_SHARED_REPO_URL`; the repo sync runs only with
  `FLEET_REPO_OVERRIDES`; the bridge is off without `BRIDGE_URL`; the App
  is optional at provisioner start.

## Decision

### 1. `FLEET_VAULT_DISABLED=1`: an explicit local-stack mode

Set on **both** core and the provisioner, like `FLEET_AUTH_DISABLED=1` on
core today.

- **Provisioner.** No vault token, CA, proxy env or egress-lock check. The
  worker gets the model token from the provisioner's own
  `CLAUDE_CODE_OAUTH_TOKEN` (a local Secret), and no `GH_TOKEN`: the forge
  takes any credentials. `repoOwner`'s github.com check goes with the owner
  vault it serves. The create answers `vault_token_applied: false`, which
  is the truth.
- **Core** accepts a definition pod without `vault_token_applied`, and skips
  `definitionWarmable`'s check that the repo URL is the GitHub repo the
  credentials name: it guards a push credential this mode never mints. Only
  in this mode.
- **It cannot reach production.** Core refuses to start with
  `FLEET_VAULT_DISABLED=1` unless `FLEET_AUTH_DISABLED=1` is set too, so the
  mode exists only where the console is already open by design. The
  provisioner refuses it while a GitHub App key is mounted, which every
  real deployment has. A buildguard test fails if any manifest under `k8s/`
  sets either variable.

A pod in this mode holds a real model token, which is what ADR-0075 exists
to prevent. On a laptop, the token is the owner's own and the cluster is
the owner's own; that is the trade this mode makes, and why it is refused
everywhere else.

### 2. The forge is data, not a mechanism

- `FLEET_SHARED_REPO_URL` points at the forge's copy of agent-fleet, so
  definitions, packs and fleet-shared come from it.
  `FLEET_DEFINITIONS_REF` names the ref, as it already does for a dev
  build.
- The `repos` rows name forge URLs. `/kind-local` writes them; the repo
  sync stays off, as it is today without `FLEET_REPO_OVERRIDES`.
- The pod's clone is local (`clone --shared` from the cache), and its
  origin is that URL, so the agent's `git push` reaches the forge. No URL
  rewriting (`insteadOf`) in any component.

### 3. The local core is reachable by a client

`local/kind/30-core.yaml` gains:

- port 9091 on the container and the Service (AgentService for clients);
- `FLEET_CLIENT_TOKEN_GAME` from the local Secret (the token `clients/game.yaml`
  names);
- `MAX_EXTERNAL_SESSIONS=2`. It defaults to 0, and at 0 no client session
  warms.

No bridge in the first cut. A client learns that a session waits from
`WaitForResult`/`Status` (ADR-0070 §4). `BRIDGE_*` joins when a local
bridge does.

### 4. Game definitions live with the game, not in agent-fleet `main`

`game-<repo>` definitions (`git.mode: push`, `branch: game/<repo>`,
`invokers: [game]`, `sidecarTools: [AskUserQuestion]`, `TodoWrite` in
`tools.allow`) and `clients/game.yaml` (`actors.allow: [MohammadBnei]`,
`announceSessions: true`) are sim-only. Production reads every file in
`definitions/` and builds a vault for each valid one. So they do not go to
agent-fleet `main`:

- they are written and reviewed in `ukubi-game`;
- `sim forge sync` commits them on a `game` branch of the forge's
  agent-fleet copy, on top of `main`;
- the local provisioner reads `FLEET_DEFINITIONS_REF=game`;
- the local core's sync validates them with the same loader, and an
  invalid file fails closed, as everywhere. `ukubi-game`'s CI runs that
  loader on them too.

### 5. `/kind-local` is repaired, not forked

The skill and `local/kind/` take this mode; there is no second local stack.
Its README's "broken since ADR-0075" note goes.

## Alternatives considered

- **A local Agent Vault.** It keeps the pod shape identical, but it needs
  the vault's image, its storage, its bootstrap and a Cilium CNP in kind,
  for a laptop where the secret being guarded is the owner's own.
- **A second provisioner (`localprov`) that spawns processes.** Rejected
  before (R11 sync): a second implementation of `ProvisionerService` drifts
  from the one production runs, and the point of the local stack is to run
  that one.
- **`insteadOf` rewriting to the forge.** It works for the provisioner by
  env, but the agent's own push needs the worker's env to carry it, which is
  code; forge URLs in the data need none.
- **Game definitions in agent-fleet `main`.** They would be unreachable in
  production (no `FLEET_CLIENT_TOKEN_GAME` there, `invokers: [game]` only),
  but production would still build their vaults, with push credentials, for
  nothing.

## Consequences

- The fleet runs on a laptop with the model API as its only outside
  dependency, and `/kind-local` works again.
- One more opt-out in core, bound to the existing one and checked in CI.
- Game definitions are not checked by agent-fleet's `repo_test.go`; they are
  checked by the same loader in the local core and in `ukubi-game`'s CI. A
  schema change in agent-fleet can break them; that surfaces at the local
  sync, not in production.
- The forge's `pushes.jsonl` is the offline record of who pushed what; the
  commit trailers stay labels.
