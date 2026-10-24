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

> **⚠ Not yet updated for `docs/adr/0075` (GitHub App + Agent Vault). The
> `GH_TOKEN` path below no longer works.** The provisioner reads no
> `GH_TOKEN`: its own git uses GitHub App installation tokens, and every
> worker pod — repo or definition — gets a token minted from Agent Vault at
> warm, so `CreateWorkerPod` fails before any Job exists unless the vault is
> reachable. A working kind run needs, on top of what this file sets up:
>
> - `GITHUB_APP_DIR` (default `/var/run/github-app`) holding the files
>   `GITHUB_APP_ID` and `GITHUB_APP_PRIVATE_KEY`, plus
>   `FLEET_GITHUB_ACCOUNTS` (comma-separated account ids whose installations
>   count);
> - an Agent Vault the provisioner can reach: `AGENT_VAULT_API_ADDR`,
>   `AGENT_VAULT_PROXY_ADDR`, and `AGENT_VAULT_SECRETS_DIR` (default
>   `/var/run/agent-vault`) holding the agent token and credential values
>   (`provisioner/internal/vault`);
> - optionally `FLEET_GIT_AUTHOR_NAME`/`FLEET_GIT_AUTHOR_EMAIL` (default
>   `ukubi`/`ukubi@bnei.dev`).
>
> The full list, with defaults, is `provisioner/internal/config/config.go`.
> None of this is in `local/kind/*.yaml` yet.

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

`agent-fleet-local-secrets` is built imperatively by the skill, never
committed, via `infisical run` wrapping `kubectl create secret` directly
(no temp file). It carries two different kinds of values and **they must
never mix**:

- `AGENTFLEET_DB_*` — **always hardcoded literals** pointing at this
  namespace's throwaway `postgres` Service (`agentfleet`/`agentfleet`/
  `agentfleetdb`). Never sourced from Infisical.
- `GH_TOKEN` / `CLAUDE_CODE_OAUTH_TOKEN` — the only two values sourced live
  from Infisical (`agent-fleet-nygh`/`dev`). Stale since `docs/adr/0075`:
  the provisioner reads no `GH_TOKEN`, and worker pods get both tokens from
  the vault (see the note at the top).

**Why this matters:** the `agent-fleet-nygh/dev` Infisical project holds the
*real* `AGENTFLEET_DB_PASSWORD` for prod's `postgres.bnei.lan`. If that ever
got exported into this local Secret, a local `core` would start claiming
and mutating **real production sessions** through a provisioner that has no
relationship to the real fleet. If you're editing the skill's secret step,
keep `AGENTFLEET_DB_*` as literals — never widen the `infisical run` scope
to include them.

`DISCORD_BOT_TOKEN` is deliberately omitted — `core/cmd/core/run.go` falls
back to a `noopNotifier` without it, same as `.claude/skills/dashboard-e2e/SKILL.md`.

## Real side effects

With working credentials against a real repo, a session **can open an
actual PR on GitHub.** Repos are rows in the `repos` table: with no
`FLEET_REPO_OVERRIDES` (as in `30-core.yaml`) core's GitHub sync is off and
the table keeps what migration 000001 seeded (`dream-analyst`,
`vos-monolith`, `agent-fleet`, `infra-bootstrap`). For routine local
iteration, insert a disposable scratch repo row rather than repeatedly
testing against the real targets.

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
| `10-workspace-hostpath.yaml` | hostPath PV + PVC named `agent-fleet-workspace` |
| `20-postgres.yaml` | throwaway Postgres (emptyDir, same creds as `dashboard-e2e`) |
| `30-core.yaml` | raw Deployment+Service for `core` (no ingress) |
| `40-provisioner.yaml` | local variant of `k8s/provisioner/deployment.yaml` (local images, no InfisicalSecret) |

`serviceaccount.yaml`/`role.yaml`/`service.yaml` for the provisioner are
applied straight from `k8s/provisioner/` — RBAC/Service shape is identical
for local and prod, so nothing here duplicates them. `kind-config.yaml` is
not a committed file — the skill generates it at cluster-create time (see
above).
