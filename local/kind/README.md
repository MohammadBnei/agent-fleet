# local/kind — local kind testing ground

Runs the real fleet (`core` + `provisioner` + real worker/sidecar `Job`s)
against a disposable [kind](https://kind.sigs.k8s.io/) cluster, so the K8s
dispatch path (RBAC, pod/Job specs, the shared-workspace mount, a worker
actually running Claude Code) can be verified without a real cluster
deploy. Driven by the `/kind-local` skill
(`.claude/skills/kind-local/SKILL.md`) — read that for the full
bring-up/teardown sequence; this file covers the parts worth understanding
before you run it.

**This directory is local-only tooling. It never touches `k8s/`, which
stays ArgoCD/Infisical/`common-app-chart`-owned prod config.**

## Offline: the real vault, a local forge (docs/adr/0080)

The stack needs no GitHub, no cluster and no Infisical; only the model API
stays online.

- **The real Agent Vault** runs as `agent-vault-local` (`15-agent-vault.yaml`,
  infra-bootstrap's image pin), never under the production Service name: the
  provisioner refuses the git rewrite against production's address.
  `vault-bootstrap.sh` registers its owner and writes the provisioner's files
  (`provisioner-agent-vault`). Pods take production's vault path, with no real
  secret in a pod.
- **The egress lock exists but is not enforced**: `06-cilium-crd.yaml` is a
  stub CRD so `k8s/provisioner/definition-egress.yaml` can apply; kind has no
  Cilium.
- **GitHub URLs stay; git rewrites them.** `repos` rows keep
  `https://github.com/...`; the provisioner's own git and every worker's
  rewrite it to the forge (`FLEET_GIT_URL_REWRITE`, ConfigMap `local-forge`),
  the worker with its session id as the push user. Owner vaults take a
  placeholder GitHub token from `FLEET_OWNER_TOKEN_FILE`.
- **Definitions come from a read-only mirror**: `mirror.sh` writes a bare repo
  on the workspace volume, outside every pod's mounts, whose `game` branch is
  the committed branch under test with `definitions/` replaced by
  `definitions/` here (the game's agents, sim-only).

## Why hostPath instead of the prod RWX PVC

Prod's `agent-fleet-workspace` PVC (`k8s/provisioner/pvc.yaml`) is
`ReadWriteMany`, shared by the provisioner and every worker/sidecar pod.
kind's default StorageClass only supports `ReadWriteOnce`, and standing up
an RWX-capable provisioner (NFS, etc.) buys nothing for a
single-developer, one-session-at-a-time local sandbox. `10-workspace-hostpath.yaml`
instead binds a static hostPath `PersistentVolume` to a PVC named exactly
`agent-fleet-workspace` — `provisioner/internal/k8s/pod.go` only ever
references the PVC by name, so every pod mounts it identically to prod with
zero code changes.

This is **not** a proposal to change prod's storage. Prod also gives each
session its own PVC holding its clone (`docs/adr/0048` §4/§5); the shared
`agent-fleet-workspace` PVC holds only the read-only repo cache and the
per-session `claude-home`. Don't read this directory as license to touch
prod's PVC.

`/data/agent-fleet-workspace` inside the kind control-plane node container
is bind-mounted to `local/kind/workspace-data/` on the Mac, via an
`extraMounts` entry in a `kind-config.yaml` generated at cluster-create time
by the skill (kind has no CLI flag for this, unlike k3d's `--volume` —
it's config-file-only, and the path needs to be absolute, which is why the
config is generated inline with `$(pwd)` baked in rather than committed as
a static file). The shared volume's contents are inspectable from the host
mid-session.
`workspace-data/` is gitignored.

## The Secret — read this before running the skill

The skill builds every Secret imperatively; none is committed, and none comes
from Infisical:

- `agent-fleet-local-secrets`: `AGENTFLEET_DB_*`, **always hardcoded
  literals** pointing at this namespace's throwaway `postgres` Service
  (`agentfleet`/`agentfleet`/`agentfleetdb`).
- `core-local-clients`: the game client's bearer, core only.
- `agent-vault-local`: the local vault's generated passwords.
- `provisioner-agent-vault`: written by `vault-bootstrap.sh`: the agent token,
  the model token (from a local file; use a separate revocable token for local
  runs) and placeholders.

**Why the literals matter:** the `agent-fleet-nygh/dev` Infisical project
holds the *real* `AGENTFLEET_DB_PASSWORD` for prod's `postgres.bnei.lan`. If
that ever got into a local Secret, a local `core` would start claiming and
mutating **real production sessions** through a provisioner that has no
relationship to the real fleet. Keep `AGENTFLEET_DB_*` as literals.

`DISCORD_BOT_TOKEN` is deliberately omitted — `core/cmd/core/run.go` falls
back to a `noopNotifier` without it, same as `.claude/skills/dashboard-e2e/SKILL.md`.

## Side effects

Pushes go to the forge, not GitHub: every `https://github.com/` is rewritten.
The forge records each push with the session id as its user; offline nothing
confines a push to its repo and branch. Repos are rows in the `repos` table:
with no `FLEET_REPO_OVERRIDES` (as in `30-core.yaml`) core's GitHub sync is
off, and the skill adds the game's repo by hand.

## Image staleness caveat

`pod.go` never sets `ImagePullPolicy` on the worker/sidecar containers it
builds dynamically, so Kubernetes' implicit default (`IfNotPresent` for a
non-`:latest` tag) applies. A stale `agent-fleet-{worker,sidecar}:local`
image will be silently reused unless you re-run `kind load docker-image`
before warming the next session after a rebuild. `core`/`provisioner`'s own
Deployments use `imagePullPolicy: Never` instead, specifically so a missed
reload fails loudly (`ErrImageNeverPull`) rather than silently.

## Files

| File | What |
|---|---|
| `00-namespace.yaml` | `agent-fleet` namespace |
| `05-traefik-crds.yaml` | the `IngressRoute` CRD, so `expose()` works |
| `06-cilium-crd.yaml` | a stub `CiliumNetworkPolicy` CRD, so the egress lock can exist (unenforced) |
| `10-workspace-hostpath.yaml` | hostPath PV + PVC named `agent-fleet-workspace` |
| `15-agent-vault.yaml` | the real Agent Vault as `agent-vault-local` (image substituted by the skill) |
| `20-postgres.yaml` | throwaway Postgres (emptyDir, same creds as `dashboard-e2e`) |
| `30-core.yaml` | raw Deployment+Service for `core` (no ingress) |
| `40-provisioner.yaml` | local variant of `k8s/provisioner/deployment.yaml` (local images, local vault, mirror, forge rewrite) |
| `vault-bootstrap.sh` | the local vault's bootstrap: owner, invite-only, the provisioner's token and files |
| `mirror.sh` | the read-only definitions mirror (`game` branch) |
| `definitions/` | the game's sim-only agents, checked by `core/internal/definitions/repo_test.go` |

`serviceaccount.yaml`/`role.yaml`/`service.yaml`/`definition-egress.yaml` for
the provisioner are applied straight from `k8s/provisioner/` — RBAC/Service shape is identical
for local and prod, so nothing here duplicates them. `kind-config.yaml` is
not a committed file — the skill generates it at cluster-create time (see
above).
