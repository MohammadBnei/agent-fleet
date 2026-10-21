# ADR-0075 — The fleet acts as a GitHub App; every pod gets its credentials from the vault

- **Status:** Proposed
- **Date:** 2026-10-20
- **Related:** [ADR-0064](0064-definition-pod-profile.md) (amended: repo pods
  take the vault half of the definition profile, plus a 12h cap),
  [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) (amended: a
  GitHub credential is a minted installation token, not an Infisical value),
  [ADR-0074](0074-fleet-repos-come-from-github.md) (amended: membership is
  the App's installations, not the bot's collaborator list),
  [ADR-0069](0069-component-auth-and-network-fences.md) (the fences the vault
  NetworkPolicy change sits next to). infra-bootstrap
  [ADR-0062](https://github.com/MohammadBnei/infra-bootstrap/blob/main/docs/adr/0062-fleet-github-app.md)
  owns the App itself, its Infisical project and the vault-side changes.

## Context

The fleet acts on GitHub as `argocd-ukubi-bot` with one classic PAT
(`agent-fleet-nygh` `/GH_TOKEN`):

- The provisioner's env holds it, and `gh auth setup-git` makes it the
  global git credential (`provisioner/internal/git/git.go:78-94`).
- Every repo worker pod gets a copy in its env (`provisioner/internal/k8s/pod.go:390`).
- It expires, and only a person can renew it.
- Membership (0074) lists the bot's collaborator repos and accepts its
  invites. Both calls are user-only endpoints.

Definition pods (0065) hold no real secret, but their GitHub credential is
still a PAT in Infisical. For Opus (`opus-tech-hq/workspace`) no PAT works:
GitHub can't create one by API, and the bot's fine-grained token 404s on a
repo where it is an outside collaborator.

infra-bootstrap ADR-0062 creates a GitHub App, `ukubi-fleet`, for the fleet
alone. Its installation tokens can be minted by API, narrowed to repository
ids and a permission subset, and last one hour. Sessions last up to 12h.

## Decision

1. **The App is the fleet's only GitHub identity.**
   - The provisioner holds the App id and key as mounted files (infra
     ADR-0062: project `fleet-github-app`). They are re-read on use, and
     never placed in an env var or argv.
   - `GITHUB_APP_` joins the reserved key prefixes in both validators
     (`core/internal/definitions/load.go`, `provisioner/internal/vault/vault.go`
     `checkSpec`), so a definition can't name the key as its secret.
2. **A forge seam.** `provisioner/internal/github` becomes the GitHub
   implementation of a `Forge` interface with two methods:
   - `ListRepos(ctx)`
   - `MintToken(ctx, owner, repoIDs, perms)`

   Forgejo is the second implementation, planned months out; the interface
   exists now on purpose. Proto, core and pods use forge-neutral names.
   - App JWT: RS256 from the stdlib, `iat` backdated 60s, `exp` ≤10 min,
     PKCS#1 and PKCS#8 keys.
   - Tokens are minted by repository id with an explicit permission subset.
3. **Only allowlisted accounts count.** The App is public. The provisioner
   config lists account ids. Installations by other accounts are ignored in
   both listing and minting, and logged; they are never deleted
   automatically.
4. **Membership is the installation (amends 0074 decisions 1 and 2).**
   - `GET /installation/repositories` for each allowlisted installation
     replaces `/user/repos?affiliation=collaborator`.
   - Invite acceptance and `FLEET_REPO_INVITERS` are removed.
   - Overrides, the shrink guard and the 7-day soft delete are unchanged.
   - The cutover happens in one step, after the App is installed on every
     live repo. Before that, check that each live repo belongs to an
     allowlisted account.
5. **The provisioner owns minting, refresh and the push to the vault
   (amends 0065).**
   - A refresh loop keyed on the `expires_at` GitHub returns re-mints when
     less than 30 min remain. It POSTs the new value to every vault that
     uses it, under `vault.Lock(name)`, and updates the sha256 memo.
   - The loop never waits on core's tick, which returns early when
     `defs.Sync` fails.
   - `reconcile` takes `GH_<VAULT>` from the minter, not from the mounted
     file, so definition YAML is unchanged.
   - On a store miss, for example just after a restart, `reconcile` mints
     synchronously.
   - **A minter failure is a reconcile error that leaves the vault as it
     is. It is never a "missing key".** A missing key narrows the vault's
     services, and the next tick then sees a widening, which revokes every
     session on the vault and ends the definition's Jobs.
   - The store never evicts. It keeps the last value until a new mint
     succeeds.
   - Agent Vault resolves the credential on each request, inside tunnels
     that are already open, so a live session picks up the new token.
6. **Definition pods** get a one-repo token (contents and pull requests
   write, metadata read) in their own vault, with the egress lock. Nothing
   else changes.
7. **Repo pods take the vault half of the definition profile (amends 0064).**
   - **Vaults.** One vault per installation owner, prefix `owner-`,
     reserved in both validators and skipped by `Prune`. Each has its own
     reconcile path:
     - passthrough for unmatched hosts;
     - **all of** `github.com:443` and `api.github.com:443` mapped to that
       owner's installation-wide token, which covers `/graphql`, `/search`
       and reads of public repos;
     - Job cleanup keyed on a repo-pod label.

     A pod uses the vault of the owner of its repo. Writes across owners
     from one pod are not supported.
   - **Permissions:** the definition set, plus issues write and actions,
     checks and statuses read. **Never `workflows` by default** (amended
     2026-10-20): GitHub Free has no push ruleset that restricts
     `.github/workflows/**`, and a workflow pushed to any same-repo branch
     runs at once with the repo's secrets and the self-hosted runner. The
     App holds `workflows`; a token carries it only for a session an admin
     opts in ("may edit workflows", migration 000014). Default branches are
     protected by rulesets the App can't bypass.
   - **Egress stays open.** Repo pods get no CiliumNetworkPolicy. The vault
     only touches the hosts it matches.
   - **The model token comes from the vault too.**
   - **Worker env:**
     - placeholders for `GH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN`;
     - `HTTPS_PROXY`;
     - one CA bundle (system roots plus the vault CA) in `SSL_CERT_FILE`,
       `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`,
       `NODE_EXTRA_CA_CERTS` and `PIP_CERT`;
     - `NO_PROXY` covering `.cluster.local`, `.svc`, `.bnei.lan`, the LAN
       and the cluster's pod and service ranges;
     - no `LEASE_ID` in the worker container.
   - **Remaining in-pod secrets**, accepted until later ADRs:
     `THOT_AUTH_TOKEN` (cluster_access) and the `request_service` DB
     passwords.
   - **Lifetime.** Repo pods are capped at 12h through
     `activeDeadlineSeconds`; a re-warm resumes the session. Mint, label and
     revoke are wired for repo Jobs.
8. **Identity from config.** Commits are authored
   `ukubi <ukubi@bnei.dev>` (`GIT_AUTHOR_*`/`GIT_COMMITTER_*`); definitions
   keep `git.identity`. The `gh api user` calls go
   (`provisioner/internal/git/git.go:85`, `worker/src/index.ts:84`), because
   `/user` refuses installation tokens.
9. **The provisioner's own git** (repo cache, `ReadDefinitions`,
   fleet-shared) carries a minted per-owner token in `GIT_CONFIG_COUNT`/
   `GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` (`http.extraHeader`) in
   `cmd.Env`. It is never in argv: `Manager.run` puts argv into its errors,
   and those reach the dashboard. Global `gh auth setup-git` goes.
10. **No fallback when the vault fails.**
    - The provisioner exports the age of the value it last pushed, per vault.
    - Alerts fire when the vault is down, when the pushed age exceeds 35
      min, and when there is no data.
    - Core derives a `CREDENTIAL_STALE` session status from a per-vault
      `pushed_at` in `ReconcileVaultsResponse`.
    - There is deliberately no env-token break-glass.

## Consequences

- No fleet pod holds a real GitHub or model credential. The exceptions are
  the two in decision 7.
- **A vault outage stops all fleet work**, admin sessions included. The
  repair path is a laptop or k9s, never a fleet session.
- **A leaked App key means write access to every repo in the allowlisted
  installations.** A stolen session token works only while its session
  lives.
- Admin sessions lose what the bot PAT could do outside the installation:
  `/user`, gists, writes to repos the App isn't installed on, and writes
  across owners from one pod.
- Every toolchain in a repo pod must trust the combined CA bundle, and
  vault throughput caps dependency installs. The spike measures both
  before repo pods move.
- An autonomous repo session still mid-turn when it reaches 12h is killed
  and not resumed automatically. `expose` previews die with the pod.
- Every session opens PRs as the same `ukubi-fleet[bot]`. ukubi-bridge never
  trusts it (ukubi-bridge #6): every bot event arrives untrusted and can't
  open or wake a thread by itself. Trusted agent-to-agent wakes go through
  core's `PublishEvent`.
- The App key's Kubernetes Secret has a distinct name. The provisioner can
  create pods that mount any Secret in the namespace, so a test asserts
  that no worker pod spec references it.
- Exfiltration through open egress is unchanged from today.

## Rollout

Each step is one PR, merged by the user.

1. **This ADR**, plus infra-bootstrap ADR-0062.
2. **Unblock Opus.** The user creates the App and installs it on every
   repo that should stay in the fleet, plus `agent-fleet` and every
   fleet-shared or genome source repo. **That full install is a hard merge
   precondition.** Installing early is harmless, because membership still
   comes from the bot until step 3. This step ships the Forge, JWT,
   allowlist, minter and refresh/push loop, `reconcile` from the minter and
   the reserved prefixes. Delete
   `GH_OPUS_WORKSPACE` and `GH_DEV_NOTES` from Infisical afterwards.
3. **Membership** from installations (decision 4), together with decision 9
   (the provisioner's own git on installation tokens): both need every
   fleet repo installed, and Opus needs neither (its cache fetch still rides
   the bot, a collaborator there). Both installations are
   `repository_selection=all` by the user's choice, so the first sync adds
   ~150 repos; the sync logs loudly when one tick grows the set that much.
4. **Spike, then repo pods.** The spike runs in a throwaway namespace. It
   covers CA trust (git, gh, curl, bun and node, go, pip and uv, cargo,
   Chromium/NSS, gRPC through MITM), passthrough, the throughput of three
   concurrent pods, and `gh` against third-party repos. Then
   infra-bootstrap's vault NetworkPolicy, limits and rate-limit change, and
   then decision 7, removing `GH_TOKEN`, `ConfigureAuth` and the worker's
   `gh api user` in the same PR.
5. **Retire the bot.** Remove `argocd-ukubi-bot` from the fleet repos and
   delete `/GH_TOKEN`.

## Step 4 as built (2026-10-21)

- **Owner vaults** are `owner-<login lowercased>`, reconciled by the
  provisioner at every repo warm and on its 5-minute refresh tick for every
  allowlisted owner, without core. A missing model token or a failed owner
  mint is an error before any vault write, so an owner vault never narrows.
  A new vault gets its `unmatched_host_policy` right after creation, for
  definition vaults too.
- **No pod clones from the remote.** The repo pods' clone init container held
  no credential and fetched its base from GitHub, so it only ever worked for
  public repos. Every pod now clones the provisioner's cache, fetched with the
  App token just before the pod is created (`EnsureRepoFetched`, never
  skipped). A repo warm therefore waits for a concurrent fetch of the same
  repo, up to its own deadline. A repo session's existing tree is kept as
  before: any `.git` means the clone is skipped, never wiped.
- **CA bundle.** The provisioner writes `bundle.pem` (its image's system roots
  plus the vault root) next to `ca.pem` in `agent-vault-ca`. Repo pods point
  every CA variable at it except `NODE_EXTRA_CA_CERTS`, which adds to Node's
  roots and takes `ca.pem`.
- **`NO_PROXY`** for repo pods: loopback, core, the vault, `.cluster.local`,
  `.svc`, `.bnei.lan`, `.bnei.dev`, `10.233.0.0/18`, `10.233.64.0/18`,
  `192.168.1.0/24`.
- **Removed:** `GH_TOKEN` and the real `CLAUDE_CODE_OAUTH_TOKEN` from every
  pod, `ConfigureAuth` and `gh` from the provisioner, the worker's
  `configureGitAuth` (`gh api user`), `EnsureRepoCachedForSession`.
- **Deferred to their own PRs:** the workflows opt-in (an owner vault is
  shared by every repo pod of its owner, so a per-session `workflows` grant
  needs a second vault per owner, not a column), and decision 10's pushed-age
  metric, alerts and `CREDENTIAL_STALE`. Until the opt-in lands, no fleet
  session can push a change under `.github/workflows/`.
- **kind-local** has no Agent Vault, so repo sessions no longer start there.
- **Egress router (follow-up PR).** A repo pod's `HTTPS_PROXY` is the
  sidecar's CONNECT router on `127.0.0.1:9093` (`sidecar/internal/egress`),
  not the vault. It sends `github.com`, `api.github.com` and
  `api.anthropic.com` on :443 to the vault with the pod's session token and
  tunnels every other host direct, untouched. The token sits in the
  sidecar's env, not the worker's; that is tidiness, not a boundary, since
  the worker can use the vault through the router anyway and a stolen token
  works only from a pod the vault's NetworkPolicy admits. The sidecar's
  telemetry git, which runs in the agent's tree, has fsmonitor, hooks,
  external diff, textconv and lazy fetches off; a `.gitattributes` clean
  filter still runs (git 2.39 has no `GIT_ATTR_SOURCE`).
  **The browser goes direct**, never through the router: through it, a page's
  script would reach GitHub and Anthropic as the fleet (the vault injects on
  the wire, and api.github.com answers CORS with `*`). Playwright starts with
  empty proxy env, so no NSS import of the vault CA is needed. Accepted
  residual: a browser the agent starts from Bash (`playwright test`) inherits
  the router, and a config with `ignoreHTTPSErrors` lets its pages reach the
  three vault hosts as the fleet. Agent Vault's MITM is HTTP/1.1 only and drops trailers
  (`internal/mitm/proxy.go:24` in v0.40.0), so this is what keeps HTTP/2 and
  gRPC working for every other host. It also takes most traffic out of the
  vault's plaintext view and rate limit. HTTP/2 and gRPC to the
  three vault hosts stay broken until Agent Vault supports them.
