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
for c in core provisioner sidecar worker migration; do
  docker build -f "$c/Dockerfile" -t "agent-fleet-${c}:local" .
  kind load docker-image "agent-fleet-${c}:local" --name agent-fleet-local
done
```

Re-run this (both the build AND the load) after any code change before
creating a new task — see `local/kind/README.md`'s image-staleness caveat:
worker/sidecar Jobs default to `imagePullPolicy: IfNotPresent`, so a
forgotten reload silently reuses the old image.

## 3. Namespace, Traefik CRDs, workspace volume, RBAC

`05-traefik-crds.yaml` supplies the `IngressRoute` CRD. kind has no Traefik,
and `expose()` creates one route per published session — without the CRD that
call fails with `the server could not find the requested resource`, so a
session can never publish a URL locally. Everything else still works; this is
not a startup dependency.

It used to be one: the provisioner created a shared `cluster-ip-whitelist`
Middleware at startup and `os.Exit(1)`d if it couldn't, so a missing CRD meant
the provisioner CrashLoopBackOffed and nothing downstream could be tested at
all. Both the Middleware and that fatal are gone (docs/adr/0048 §6).

```bash
kubectl apply -f local/kind/00-namespace.yaml
kubectl apply -f local/kind/05-traefik-crds.yaml
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

## 5. Postgres, then migration, then core

```bash
kubectl apply -f local/kind/20-postgres.yaml
kubectl wait --for=condition=ready pod -l app=postgres -n agent-fleet --timeout=60s

# The dedicated `migration` image (docs/adr/0030) — same image prod's
# ArgoCD PreSync hook runs, just a one-off Pod here instead of a Job. Runs
# BEFORE core starts, not after: core no longer applies or embeds any
# schema itself, so a stale/missing migration would surface as core
# immediately failing every query against a table/column that doesn't
# exist yet, not as a `core migrate` step you could retry independently.
kubectl run agent-fleet-migrate -n agent-fleet --restart=Never \
  --image=agent-fleet-migration:local --image-pull-policy=IfNotPresent \
  --command -- migrate \
  -path /migrations \
  -database "postgres://agentfleet:agentfleet@postgres:5432/agentfleetdb?sslmode=disable" \
  up
kubectl wait --for=condition=ready pod/agent-fleet-migrate -n agent-fleet --timeout=60s || true
kubectl logs -n agent-fleet agent-fleet-migrate
kubectl delete pod -n agent-fleet agent-fleet-migrate

kubectl apply -f local/kind/30-core.yaml
kubectl wait --for=condition=available deploy/core -n agent-fleet --timeout=60s
```

## 6. Provisioner

```bash
kubectl apply -f local/kind/40-provisioner.yaml
kubectl wait --for=condition=available deploy/provisioner -n agent-fleet --timeout=60s

# git.Manager.ConfigureAuth runs `gh auth setup-git` at startup — check this
# didn't fail on a bad/missing GH_TOKEN before creating a task.
kubectl logs -n agent-fleet deploy/provisioner --tail=50
```

## 7. Smoke test — open a real session

```bash
kubectl port-forward -n agent-fleet svc/core 8080:8080 &
CORE_PF_PID=$!
until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 0.5; done
```

**Real side effect**: a session against a real repo with a real `GH_TOKEN`
can open an actual PR on GitHub — the agent runs `git push`/`gh pr create`
itself, whenever it decides to. Target repos live in the `repos` table
(seeded by the migration, editable from the dashboard), so for routine
iteration prefer a disposable scratch repo added there over the real ones.

**Two calls, not one** (docs/adr/0048). `CreateSession` makes a row and
nothing else — no pod, no directory. The FIRST MESSAGE is what provisions,
and that ordering is load-bearing: `resumeFromSeq` is computed from
`LatestSeq` at provisioning time, so a message appended before the pod
exists lands below its cursor and is never delivered. There is no dispatch
loop to wait on any more.

```bash
SID=$(curl -s http://localhost:8080/agentfleet.v1.DashboardService/CreateSession \
  -H 'Content-Type: application/json' -H 'X-Fleet-Dashboard: 1' \
  -d '{"repo": "<known-repo>", "description": "<label>", "title": "<label>"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['session']['id'])")

# This is what boots the pod.
curl -s http://localhost:8080/agentfleet.v1.DashboardService/PostMessage \
  -H 'Content-Type: application/json' -H 'X-Fleet-Dashboard: 1' \
  -d "{\"sessionId\": \"$SID\", \"text\": \"<what you want it to do>\"}"
```

A session does not end on its own — it replies and then idles, waiting for
the next message, until `StopSession` or the idle timeout. Don't wait for a
Job to Complete; send a stop when you're done with it:

```bash
kubectl get pods -n agent-fleet -w

curl -s http://localhost:8080/agentfleet.v1.DashboardService/StopSession \
  -H 'Content-Type: application/json' -H 'X-Fleet-Dashboard: 1' \
  -d "{\"sessionId\": \"$SID\"}"
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
