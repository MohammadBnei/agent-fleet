// Same for(;;) { ...; sleep(N) } shape as worker/src/index.ts's task-claim
// loop — no queue library, no k8s informer/controller framework, just a
// plain poll, matching this codebase's existing simplicity bar.
import { runningSessions, sessionsNeedingTeardown, setSessionStatus } from "./db.js";
import { deleteE2eResources, listE2ePodsByLabel } from "./k8s.js";
import { dropClient } from "./mcpProxy.js";
import { log } from "./log.js";

const POLL_INTERVAL_MS = Number(process.env.RECONCILE_INTERVAL_MS ?? 10000);

async function tearDown(session: { id: string; task_id: string }): Promise<void> {
  await deleteE2eResources(session.task_id);
  dropClient(session.task_id);
  await setSessionStatus(session.id, "torn_down");
  log("info", "e2e session torn down", { taskId: session.task_id, sessionId: session.id });
}

// On its own restart, cross-check real cluster state against the DB so a
// crash between "pod created" and "next reconcile tick" never leaks a pod
// nothing is watching for anymore.
async function reconcileOrphans(): Promise<void> {
  const [livePods, tracked] = await Promise.all([listE2ePodsByLabel(), runningSessions()]);
  const trackedTaskIds = new Set(tracked.map((s) => s.task_id));
  for (const pod of livePods) {
    if (!trackedTaskIds.has(pod.taskId)) {
      log("info", "cleaning up orphaned e2e pod on startup", { taskId: pod.taskId, podName: pod.podName });
      await deleteE2eResources(pod.taskId);
    }
  }
}

export async function runReconcileLoop(): Promise<void> {
  await reconcileOrphans();
  for (;;) {
    const due = await sessionsNeedingTeardown();
    for (const session of due) {
      try {
        await tearDown(session);
      } catch (err) {
        log("error", "failed tearing down e2e session", { taskId: session.task_id, error: String(err) });
      }
    }
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
}
