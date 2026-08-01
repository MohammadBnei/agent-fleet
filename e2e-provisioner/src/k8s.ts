import * as k8s from "@kubernetes/client-node";
import type { TaskRef } from "./db.js";

const NAMESPACE = process.env.NAMESPACE ?? "agent-fleet";
const E2E_RUNNER_IMAGE = process.env.E2E_RUNNER_IMAGE ?? "mohammaddocker/agent-fleet-e2e-runner:latest";
// No wildcard DNS exists anywhere in this cluster (ADR-0001) — one static,
// already-provisioned hostname with path-based routing per task, not a
// per-task subdomain. The DNS record itself is a one-time human setup step
// (see this package's README), not something this code can create.
const E2E_HOST = process.env.E2E_HOST ?? "e2e.bnei.dev";
// Reused across every session instead of minting a per-task Secret — same
// LAN-admin credential already gates pgweb/Alertmanager.
const BASIC_AUTH_MIDDLEWARE = { name: "basic-admin-auth", namespace: "default" };

const APP_PORT = 3000;
const CODE_SERVER_PORT = 8080;
const PLAYWRIGHT_PORT = 8931;

// Per-repo build/run command. Env-overridable (E2E_START_CMD_<REPO>, repo
// name upper-snake-cased) rather than a config file — two repos today, not
// worth more than a lookup with an override escape hatch.
const REPO_START_CMD: Record<string, string> = {
  "dream-analyst": process.env.E2E_START_CMD_DREAM_ANALYST ?? "bun install && bun run dev",
  "vos-monolith": process.env.E2E_START_CMD_VOS_MONOLITH ?? "bun install && bun run dev",
};

function startCmdFor(repo: string): string {
  const cmd = REPO_START_CMD[repo];
  if (!cmd) throw new Error(`no e2e start command configured for repo "${repo}"`);
  return cmd;
}

// common-app-chart's pvc.yaml names the PVC after the Release name, which
// equals the ApplicationSet element name — "dream-analyst-worker" /
// "vos-monolith-worker" for the two existing worker Applications.
function workspacePvcFor(repo: string): string {
  return `${repo}-worker`;
}

// Short, DNS-safe, deterministic per task — collision odds on a 20-hex-char
// slice of a UUID are not worth guarding against here.
function resourceName(taskId: string): string {
  return `e2e-${taskId.replace(/-/g, "").slice(0, 20)}`;
}

function kubeClients() {
  const kc = new k8s.KubeConfig();
  kc.loadFromCluster();
  return {
    core: kc.makeApiClient(k8s.CoreV1Api),
    custom: kc.makeApiClient(k8s.CustomObjectsApi),
  };
}

export interface CreatedE2ePod {
  podName: string;
  ingressPath: string;
}

export async function createE2ePod(task: TaskRef): Promise<CreatedE2ePod> {
  const { core, custom } = kubeClients();
  const name = resourceName(task.id);
  const labels = {
    "agent-fleet.dev/component": "e2e-runner",
    "agent-fleet.dev/task-id": task.id,
  };

  const pod: k8s.V1Pod = {
    metadata: { name, namespace: NAMESPACE, labels },
    spec: {
      restartPolicy: "Never",
      containers: [
        {
          name: "e2e-runner",
          image: E2E_RUNNER_IMAGE,
          env: [
            { name: "E2E_WORKTREE_PATH", value: "/workspace" },
            { name: "E2E_START_CMD", value: startCmdFor(task.repo) },
            { name: "E2E_APP_PORT", value: String(APP_PORT) },
            { name: "E2E_CODE_SERVER_PORT", value: String(CODE_SERVER_PORT) },
            { name: "E2E_PLAYWRIGHT_PORT", value: String(PLAYWRIGHT_PORT) },
          ],
          ports: [
            { containerPort: APP_PORT, name: "app" },
            { containerPort: CODE_SERVER_PORT, name: "code-server" },
            { containerPort: PLAYWRIGHT_PORT, name: "playwright" },
          ],
          volumeMounts: [
            { name: "workspace", mountPath: "/workspace", subPath: `worktrees/${task.id}` },
            { name: "dshm", mountPath: "/dev/shm" },
          ],
        },
      ],
      volumes: [
        { name: "workspace", persistentVolumeClaim: { claimName: workspacePvcFor(task.repo) } },
        // Headless Chromium in a container needs more than the default 64Mi
        // /dev/shm — an emptyDir with a size cap, not SYS_ADMIN/no-seccomp.
        { name: "dshm", emptyDir: { medium: "Memory", sizeLimit: "1Gi" } },
      ],
    },
  };
  await core.createNamespacedPod({ namespace: NAMESPACE, body: pod });

  const service: k8s.V1Service = {
    metadata: { name, namespace: NAMESPACE, labels },
    spec: {
      selector: { "agent-fleet.dev/task-id": task.id },
      ports: [
        { name: "app", port: APP_PORT },
        { name: "code-server", port: CODE_SERVER_PORT },
        { name: "playwright", port: PLAYWRIGHT_PORT },
      ],
    },
  };
  await core.createNamespacedService({ namespace: NAMESPACE, body: service });

  const stripPrefixName = `${name}-stripprefix`;
  await custom.createNamespacedCustomObject({
    group: "traefik.io",
    version: "v1alpha1", // confirm against the cluster's installed Traefik CRD version
    namespace: NAMESPACE,
    plural: "middlewares",
    body: {
      apiVersion: "traefik.io/v1alpha1",
      kind: "Middleware",
      metadata: { name: stripPrefixName, namespace: NAMESPACE, labels },
      spec: { stripPrefix: { prefixes: [`/${task.id}/app`, `/${task.id}/code`] } },
    },
  });

  await custom.createNamespacedCustomObject({
    group: "traefik.io",
    version: "v1alpha1",
    namespace: NAMESPACE,
    plural: "ingressroutes",
    body: {
      apiVersion: "traefik.io/v1alpha1",
      kind: "IngressRoute",
      metadata: { name, namespace: NAMESPACE, labels },
      spec: {
        entryPoints: ["websecure"],
        tls: { certResolver: "le" },
        routes: [
          {
            match: `Host(\`${E2E_HOST}\`) && PathPrefix(\`/${task.id}/app\`)`,
            kind: "Rule",
            services: [{ name, namespace: NAMESPACE, port: APP_PORT }],
            middlewares: [
              { name: stripPrefixName, namespace: NAMESPACE },
              BASIC_AUTH_MIDDLEWARE,
            ],
          },
          {
            match: `Host(\`${E2E_HOST}\`) && PathPrefix(\`/${task.id}/code\`)`,
            kind: "Rule",
            services: [{ name, namespace: NAMESPACE, port: CODE_SERVER_PORT }],
            middlewares: [
              { name: stripPrefixName, namespace: NAMESPACE },
              BASIC_AUTH_MIDDLEWARE,
            ],
          },
        ],
      },
    },
  });

  return { podName: name, ingressPath: `/${task.id}` };
}

// The Playwright MCP endpoint is only ever reached in-cluster (by this
// service, proxying on the agent's behalf — see mcpProxy.ts), never through
// the human-facing IngressRoute, so it needs no path/middleware of its own.
export function playwrightUrlFor(taskId: string): string {
  const name = resourceName(taskId);
  return `http://${name}.${NAMESPACE}.svc.cluster.local:${PLAYWRIGHT_PORT}/mcp`;
}

export function previewUrlFor(taskId: string): string {
  return `https://${E2E_HOST}/${taskId}/app/`;
}

export async function deleteE2eResources(taskId: string): Promise<void> {
  const { core, custom } = kubeClients();
  const name = resourceName(taskId);
  const ignore404 = (err: unknown) => {
    if (err && typeof err === "object" && "code" in err && (err as { code: number }).code === 404) return;
    throw err;
  };
  await custom
    .deleteNamespacedCustomObject({ group: "traefik.io", version: "v1alpha1", namespace: NAMESPACE, plural: "ingressroutes", name })
    .catch(ignore404);
  await custom
    .deleteNamespacedCustomObject({ group: "traefik.io", version: "v1alpha1", namespace: NAMESPACE, plural: "middlewares", name: `${name}-stripprefix` })
    .catch(ignore404);
  await core.deleteNamespacedService({ name, namespace: NAMESPACE }).catch(ignore404);
  await core.deleteNamespacedPod({ name, namespace: NAMESPACE }).catch(ignore404);
}

export interface LiveE2ePod {
  taskId: string;
  podName: string;
}

// Startup reconciliation (src/reconcile.ts) cross-checks this against the
// DB's runningSessions() to clean up anything orphaned by a previous crash.
export async function listE2ePodsByLabel(): Promise<LiveE2ePod[]> {
  const { core } = kubeClients();
  const { items } = await core.listNamespacedPod({
    namespace: NAMESPACE,
    labelSelector: "agent-fleet.dev/component=e2e-runner",
  });
  return items
    .map((pod) => ({
      taskId: pod.metadata?.labels?.["agent-fleet.dev/task-id"],
      podName: pod.metadata?.name,
    }))
    .filter((p): p is LiveE2ePod => Boolean(p.taskId && p.podName));
}
