---
name: kind-local
description: Spin up a disposable kind cluster running the real fleet (core + provisioner + real worker/sidecar Jobs) to test the Kubernetes dispatch path end-to-end, then tear it all down. Use when the user wants to test a provisioner/core/worker/sidecar change against real pod dispatch without deploying to the real cluster, or verify a worker Job actually runs and opens a PR.
user-invocable: true
allowed-tools:
  - Read
  - Write
  - Edit
  - Bash(docker *)
  - Bash(kind *)
  - Bash(kubectl *)
  - Bash(infisical *)
  - Bash(gh *)
  - Bash(curl *)
  - Bash(mkdir *)
  - Bash(rm *)
  - Bash(lsof *)
---

# /kind-local — local kind testing ground for the real fleet

Unlike `/dashboard-e2e` (which runs `core` bare-metal with zero Kubernetes
involvement), this spins up a real [kind](https://kind.sigs.k8s.io/)
cluster and runs `core`, `provisioner`, and real worker/sidecar `Job`s — the
only way to test RBAC, pod/Job specs, the shared-workspace mount, or an
actual worker run without deploying to `ukubi-cluster`. Read
`local/kind/README.md` first — it has the Secret safety rule and the
real-side-effect warning this skill depends on.

**Always run section 8 (teardown), even if an earlier step failed** — a
stray kind cluster or a leftover port-forward will break the next run.

## 1. Create the cluster

Native sidecar containers (`provisioner/internal/k8s/pod.go`'s init
container with `restartPolicy: Always`) need K8s ≥1.29. Always create a new,
dedicated cluster — don't repurpose whatever `kind get clusters` already
shows, since the hostPath mount can only be set at creation time.

kind has no CLI flag for a host bind mount (unlike k3d's `--volume`) — it's
config-file-only, and the path must be absolute, so the config is generated
inline rather than committed as a static file:

```bash
mkdir -p local/kind/workspace-data
cat > /tmp/agent-fleet-kind-config.yaml <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: $(pwd)/local/kind/workspace-data
        containerPath: /data/agent-fleet-workspace
EOF

kind create cluster --name agent-fleet-local \
  --image kindest/node:v1.31.4 \
  --config /tmp/agent-fleet-kind-config.yaml
```

(If `v1.31.4` isn't available for your installed kind version, check `kind
version` / the [kind releases page](https://github.com/kubernetes-sigs/kind/releases)
for the current default node image tag — any ≥1.29 works.)

## 2. Build and import images

`${c}` bracing is required here, not `$c:local` — in zsh, `$c:l` parses as
the lowercase history modifier and silently mangles the tag to
`agent-fleet-coreocal` (confirmed live: `kind load` then fails with `image
"agent-fleet-coreocal" not present locally`).

```bash
for c in core provisioner sidecar worker; do
  docker build -f "$c/Dockerfile" -t "agent-fleet-${c}:local" .
  kind load docker-image "agent-fleet-${c}:local" --name agent-fleet-local
done
```

Re-run this (both the build AND the load) after any code change before
creating a new task — see `local/kind/README.md`'s image-staleness caveat:
worker/sidecar Jobs default to `imagePullPolicy: IfNotPresent`, so a
forgotten reload silently reuses the old image.

## 3. Namespace, workspace volume, RBAC

```bash
kubectl apply -f local/kind/00-namespace.yaml
kubectl apply -f local/kind/10-workspace-hostpath.yaml
kubectl apply -n agent-fleet \
  -f k8s/provisioner/serviceaccount.yaml \
  -f k8s/provisioner/role.yaml \
  -f k8s/provisioner/service.yaml
```

## 4. The Secret

**Read `local/kind/README.md`'s Secret section before touching this step.**
`AGENTFLEET_DB_*` are always the hardcoded literals below — never sourced
from Infisical. Only `GH_TOKEN`/`CLAUDE_CODE_OAUTH_TOKEN` come from the
`infisical run` wrapper.

```bash
infisical run --domain=https://infisical.bnei.dev/api \
  --projectId=ae771c2c-5115-452a-8f1c-1e03fa0e2b9a --env=dev -- \
  bash -c 'kubectl create secret generic agent-fleet-local-secrets -n agent-fleet \
    --from-literal=AGENTFLEET_DB_HOST=postgres \
    --from-literal=AGENTFLEET_DB_PORT=5432 \
    --from-literal=AGENTFLEET_DB_NAME=agentfleetdb \
    --from-literal=AGENTFLEET_DB_USER=agentfleet \
    --from-literal=AGENTFLEET_DB_PASSWORD=agentfleet \
    --from-literal=GH_TOKEN="$GH_TOKEN" \
    --from-literal=CLAUDE_CODE_OAUTH_TOKEN="$CLAUDE_CODE_OAUTH_TOKEN"'
```

## 5. Postgres, then core + migration

```bash
kubectl apply -f local/kind/20-postgres.yaml
kubectl wait --for=condition=ready pod -l app=postgres -n agent-fleet --timeout=60s

kubectl apply -f local/kind/30-core.yaml
kubectl wait --for=condition=available deploy/core -n agent-fleet --timeout=60s

# core's own `migrate` subcommand (same as prod's ArgoCD PreSync hook) —
# distroless has no shell, but `exec ... -- /core migrate` execs the binary
# directly, no shell needed.
kubectl exec -n agent-fleet deploy/core -- /core migrate
```

## 6. Provisioner

```bash
kubectl apply -f local/kind/40-provisioner.yaml
kubectl wait --for=condition=available deploy/provisioner -n agent-fleet --timeout=60s

# git.Manager.ConfigureAuth runs `gh auth setup-git` at startup — check this
# didn't fail on a bad/missing GH_TOKEN before creating a task.
kubectl logs -n agent-fleet deploy/provisioner --tail=50
```

## 7. Smoke test — create a real task

```bash
kubectl port-forward -n agent-fleet svc/core 8080:8080 &
CORE_PF_PID=$!
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 0.5; done
```

**Real side effect**: creating a task against `dream-analyst`/`vos-monolith`
(`core/internal/tasks/store.go`'s `KnownRepos`) with a real `GH_TOKEN` opens
an actual PR on GitHub once the worker completes. For routine iteration,
prefer a disposable scratch repo added as a third `KnownRepos` entry (same
mechanism `/fleet-ops` documents for onboarding any repo) over repeatedly
hitting the two real targets.

Create the task via the dashboard UI at `http://localhost:8080`, or
directly against the ConnectRPC endpoint:

```bash
curl -s http://localhost:8080/agentfleet.v1.DashboardService/CreateTask \
  -H 'Content-Type: application/json' \
  -d '{"repo": "<known-repo>", "description": "<task description>"}'
```

Then watch dispatch happen (core's loop polls every 2s):

```bash
kubectl get pods -n agent-fleet -w
```

Once a `Job` appears, follow both containers — the worker container only
starts after the sidecar's `/readyz` passes:

```bash
kubectl logs -n agent-fleet job/<taskId> -c sidecar -f
kubectl logs -n agent-fleet job/<taskId> -c worker -f
```

Confirm the Job reaches `Completed`, then check the real PR:

```bash
gh pr list --repo <target-repo-url>
```

## 8. Teardown — run this every time, no exceptions

Three explicit levels — pick based on whether you're iterating again soon:

```bash
kill "$CORE_PF_PID" 2>/dev/null   # stop the port-forward from step 7
rm -f /tmp/agent-fleet-kind-config.yaml

kubectl delete namespace agent-fleet           # Level 1: iterate again soon, keep cluster/images
# --- or ---
kind delete cluster --name agent-fleet-local   # Level 2: full reset, keeps Mac-side workspace data
# --- or, additionally ---
rm -rf local/kind/workspace-data               # Level 3: also wipes worktrees/clones on the Mac side
```

Verify nothing's left before ending the session:

```bash
kind get clusters                                                   # expect agent-fleet-local gone (Level 2/3) or present (Level 1)
docker ps --filter "label=io.x-k8s.kind.cluster=agent-fleet-local"  # expect empty after Level 2/3
lsof -i :8080                                                        # expect empty
```
