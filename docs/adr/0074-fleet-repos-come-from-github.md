# ADR-0074 — Fleet repos come from GitHub; overrides come from infra-bootstrap

- **Status:** Accepted
- **Date:** 2026-10-19
- **Related:** [ADR-0028](0028-dynamic-repo-config.md) (amended: the `repos`
  table is no longer dashboard-editable; it is a projection of GitHub plus an
  overrides file), [ADR-0037](0037-thot-is-a-worker-task.md) (unchanged
  meaning of `cluster_access`; only where the human decision is recorded
  moves), [ADR-0062](0062-defined-agents.md) (`workspace.repo` still names a
  `repos` row), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md)
  (the bot's `GH_TOKEN` the provisioner already holds is the one this uses),
  [ADR-0069](0069-component-auth-and-network-fences.md) (RBAC fences this
  adds a test to). The overrides file is delivered by infra-bootstrap's
  `bootstrap` Application, like `fleet-oidc-secret`.

## Context

A repo joins the fleet today by hand, in three places: invite
`argocd-ukubi-bot` as a collaborator on GitHub, accept the invite as the bot,
and create a `repos` row in the console (name, URL, base branch, image,
cluster access). The last two steps are the ones that get forgotten or done
wrong: infra-bootstrap was seeded with an SSH URL (`ErrSSHURL`,
`core/internal/repos/store.go:22`), a repo accepted by the bot but missing a
row is invisible, and a row whose collaborator access was revoked fails at
clone. The bot's collaborator list on GitHub is already the real access
list, because the bot's token is what clones and pushes. The row
duplicates it.

`cluster_access` is the exception. It gives a repo's sessions the kubectl
shim to `thot-executor` (ADR-0037), and ADR-0037 calls it "a human's
decision". Today that decision is a console toggle with no review and no
history.

## Decision

1. **The collaborator list is the switch.** A repo is in the fleet while
   `argocd-ukubi-bot` is a collaborator on it, and leaves when the bot is
   removed. There is no second opt-in signal: no topic, label or file.

2. **The provisioner discovers; core projects.** The provisioner already
   holds the bot token. On core's 60s tick, a new provisioner RPC
   `ListGitHubRepos`, next to `ListDefinitionFiles`, does two things.
   - It accepts pending repository invitations, but only when the
     **inviter** is on `FLEET_REPO_INVITERS`. The initial value is
     `MohammadBnei`. Any other invitation is left pending and logged at
     WARN, and is never declined.
   - It lists `GET /user/repos?affiliation=owner,collaborator`, following
     every page. `organization_member` is not included, because org
     membership must not imply fleet membership. Archived repos and forks
     are skipped.

   Each entry it returns has the GitHub `id`, `full_name`, `clone_url`
   (always HTTPS) and `default_branch`. Core upserts `repos` from that list.

3. **Identity is the GitHub repo id.** `repos` gains `github_id BIGINT
   UNIQUE`, `full_name TEXT` and `gone_since TIMESTAMPTZ`. A rename changes
   `full_name` and `url` on the same row. `name` stays the primary key
   and the fleet-facing handle (definitions and sessions reference it). By
   default it is the GitHub repo name. Two repos with the same short name
   collide. When that happens, the second one is not inserted and core logs
   an ERROR naming both full names, until an override gives one of them a
   `name`. A short name is never inferred from an owner prefix.

4. **Base branch is GitHub's `default_branch`.** It is read on every tick,
   so a branch rename is picked up without anyone editing anything. An
   override `baseBranch` wins, for the rare repo whose fleet base is not
   its default.

5. **Overrides live in infra-bootstrap, delivered by ArgoCD.** The file
   `gitops/bootstrap/fleet-repo-overrides.yaml` is a ConfigMap
   `fleet-repo-overrides` in namespace `agent-fleet`, with one key,
   `overrides.yaml`:

   ```yaml
   repos:
     MohammadBnei/infra-bootstrap:
       id: 1234567          # required; must match the GitHub repo id
       clusterAccess: true
     opus-tech-hq/workspace:
       id: 7654321
       name: opus-workspace
   ```

   The keys are `owner/repo`. The allowed fields are `id` (required),
   `name`, `baseBranch`, `image` and `clusterAccess`. Core mounts the
   ConfigMap as a directory (`optional: true`, no `subPath`, so updates
   propagate) and re-reads it on every tick.

   - If the `id` does not match the live repo's id, the entry is ignored and
     core logs an ERROR. A deleted repo whose name was re-created by someone
     else inherits nothing.
   - An unknown field logs a WARN and is otherwise ignored.
   - A YAML error keeps the last good copy and logs an ERROR.
   - A **mounted but empty** file is valid and means "no overrides". This is
     the way to revoke every grant.
   - A **missing** file, meaning a mount problem, keeps the last good copy.
     With no good copy yet in this process (a core restart while the
     ConfigMap is absent), the sync does not run at all and every row keeps
     its stored values. An absent file therefore never revokes a grant;
     only an empty one does.
   - `FLEET_REPO_OVERRIDES` unset (kind, local runs) turns the sync off.

   **`cluster_access` can only be set from this file.** Its default is
   `false`, so a repo that only appears on GitHub never gets it, and
   removing a repo's entry revokes it on the next tick.

6. **Removal is soft first.** When a repo is missing from a complete listing,
   it gets `gone_since = now()`. A gone repo is hidden from the console's
   pickers, and warms and creates against it are refused. It is
   hard-deleted when **both** of these hold:
   - `gone_since` is older than 7 days
   - no non-archived session references it

   If it reappears (the bot is re-added), `gone_since` is cleared and the row
   stays as it was.

   A listing counts as complete only when every page returned 2xx. A failed
   or partial listing changes nothing.

   **Shrink guard:** if one tick would mark more than 2 repos gone, it marks
   none, and core logs an ERROR with the list. A human then checks whether
   the token or GitHub is the problem before anything is removed.

7. **The console is read-only for repos.** `CreateRepo`, `UpdateRepo` and
   `DeleteRepo` are removed from `DashboardService`. Every write to `repos`
   now comes from the sync. `SyncRepo` stays, because it only warms the
   clone cache. The repo view shows `full_name`, the effective base branch,
   image and cluster access, and how to add or remove a repo. Gone repos are
   not listed.

8. **Cutover.** Before the first sync, the overrides file is seeded from a
   dump of the live rows. Every row that differs from the GitHub defaults
   gets an entry: the cluster-access repos, non-default base branches,
   custom images, and names that differ from the GitHub name. The first sync
   matches the existing rows by URL and fills in `github_id`, so no row is
   recreated and no session loses its repo.

9. **An RBAC test guards the file.** The file is now a privilege grant, so
   a unit test over `k8s/**/role*.yaml` fails if any agent-fleet Role or
   ClusterRole gains `update`, `patch` or `delete` on ConfigMaps without a
   `resourceNames` list that excludes `fleet-repo-overrides`. Today the
   provisioner can `create` ConfigMaps and update only `agent-vault-ca`
   (`k8s/provisioner/role.yaml:57-60`), and worker pods have no
   service-account token. `create` cannot be pinned by `resourceNames`, so
   the provisioner could create the ConfigMap while it is absent; ArgoCD
   owns the object, so this is the named residual.

## Alternatives considered

- **Keep the console as the editor and only automate invites.** This is the
  smallest change, but it leaves `cluster_access` an unreviewed toggle and
  keeps a second list that drifts from GitHub.
- **Event-driven discovery, from GitHub webhooks through ukubi-bridge.** It
  would pick up changes instantly, not within 60s. Rejected for now: the
  bridge is not built, and collaborator events on a personal account need
  a webhook on every repo. Polling and the data model here do not stand in
  its way: a webhook can later trigger the same sync.
- **An opt-in marker, such as a topic or `.fleet.yaml`.** It guards against
  the bot being a collaborator for some reason other than the fleet. No
  such repo exists today; has-journal joins at cutover.
- **Overrides inside each repo (`.fleet.yaml`).** Rejected: anyone with push
  access to the repo could grant `cluster_access` to themselves.
- **Overrides read from git by the provisioner (like definitions).**
  Rejected for delivery. Every other piece of cluster config arrives
  through ArgoCD. Reading from git would also need the provisioner to read
  infra-bootstrap at a pinned revision, a second git path for one small
  file. Cost of the choice made instead: core can only log the ConfigMap's
  `resourceVersion`, not the source commit.

## Consequences

- Adding the bot as a collaborator is the whole onboarding. The repo
  shows up in the console within one tick of the invite being accepted,
  which takes at most two ticks, so about 2 minutes.
- Removing the bot hides the repo within one tick and deletes it after 7
  days, unless sessions still reference it.
- An override takes effect within about 3 minutes of its merge:
  - ArgoCD sync
  - kubelet ConfigMap propagation (up to about 60s)
  - one core tick
- A ConfigMap edit by anyone with write access in `agent-fleet` takes effect
  until ArgoCD's self-heal reverts it. Decision 9 keeps fleet components out
  of that set. Cluster admins are in it, and they have the cluster anyway.
- Changing a repo's `name` override renames its row. Sessions reference
  repos by name with no foreign key, so open sessions on the old name lose
  their repo. Rename only when no session on it is open.
- The provisioner makes about 2 GitHub API calls per tick (invitations, plus
  one page of repos per 100). That is about 3,000 per day, well inside the
  token's 5,000 per hour.
- The bot's token is a single point. When it expires (the current one does
  on 2026-10-28), listing fails, the sync changes nothing (decision 6), and
  clones fail as they would today.
- Repo edits gain an audit trail: git history in infra-bootstrap for
  overrides, and GitHub's collaborator log for membership.

## The bet

The bot's collaborator list matches the operator's intent. Nobody adds
`argocd-ukubi-bot` to a repo for some other reason without also wanting
that repo in the fleet. If that stops being true, the fix is an opt-in
marker added on top of decision 1. Nothing else in this ADR changes.

## Not doing

- Webhook-driven sync (later, through the bridge).
- Per-repo opt-in markers.
- Declining invitations from unknown inviters. They stay pending, for a
  human to handle.
- Moving `image` defaults out of core's catalog.
- Any change to how definitions reference repos.
