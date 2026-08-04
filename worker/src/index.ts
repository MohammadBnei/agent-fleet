import { claimNextTask, setTaskStatus, appendJournal, updateHeartbeat, requeueTransient, stillHoldsLease } from "./db.js";
import {
  configureGitAuth,
  ensureRepoCloned,
  createWorktree,
  removeWorktree,
  pushAndOpenPr,
} from "./git.js";
import { postReply } from "./discord.js";
import { runPlanningPhase, runImplementationPhase, TransientError } from "./planning.js";
import { log } from "./log.js";

const TARGET_REPO = process.env.TARGET_REPO;
const TARGET_REPO_URL = process.env.TARGET_REPO_URL;
const WORKER_NAME = process.env.WORKER_NAME ?? `${TARGET_REPO}-worker`;
const POLL_INTERVAL_MS = Number(process.env.POLL_INTERVAL_MS ?? 5000);
const HEARTBEAT_INTERVAL_MS = 30_000;
// A crashed/stuck task gets this many automatic reclaim-or-transient-error
// retries before it's given up on as terminally failed. See docs/adr/0016.
const MAX_TASK_RETRIES = Number(process.env.MAX_TASK_RETRIES ?? 2);

if (!TARGET_REPO || !TARGET_REPO_URL) {
  throw new Error("TARGET_REPO and TARGET_REPO_URL must be set");
}

async function handleTask(task: Awaited<ReturnType<typeof claimNextTask>>): Promise<void> {
  if (!task) return;
  const branch = `agent/${task.id}`;
  log("info", "task claimed", { worker: WORKER_NAME, taskId: task.id, description: task.description });
  await appendJournal(task.repo, WORKER_NAME, "task.claimed", { taskId: task.id });

  if (task.retry_count >= MAX_TASK_RETRIES) {
    await removeWorktree(task.id, branch);
    await setTaskStatus(task.id, "failed");
    const error = `exceeded retry cap (${task.retry_count}) after repeated crashes/transient errors`;
    await appendJournal(task.repo, WORKER_NAME, "task.failed", { taskId: task.id, error });
    if (task.discord_thread_id) await postReply(task.discord_thread_id, `Task failed: ${error}`);
    log("error", "task exceeded retry cap", { worker: WORKER_NAME, taskId: task.id, retryCount: task.retry_count });
    return;
  }

  const leaseId = task.lease_id as string;
  const heartbeat = setInterval(() => { updateHeartbeat(task.id).catch(() => {}); }, HEARTBEAT_INTERVAL_MS);
  let requeued = false;
  // Only a reclaimed task caught mid-implementation, with a session to
  // resume, skips straight back into implementation — every other reclaim
  // (mid-planning, mid-claim) restarts clean, since planning is cheap and
  // human-supervised, unlike implementation (where the real incident died).
  const resuming = task.status === "implementing" && Boolean(task.planning_session_id);

  try {
    await createWorktree(task.id, resuming);

    let planningSessionId = task.planning_session_id ?? "";
    if (!resuming) {
      await setTaskStatus(task.id, "planning");
      if (task.discord_thread_id) {
        await postReply(
          task.discord_thread_id,
          `Starting planning for **${task.description}** — reply here to weigh in, and say "approved" when you're happy with the plan.`,
        );
      }

      const planningResult = await runPlanningPhase(task);
      if (planningResult.aborted) {
        log("info", "task cancelled", { worker: WORKER_NAME, taskId: task.id, phase: "planning" });
        await setTaskStatus(task.id, "cancelled");
        await appendJournal(task.repo, WORKER_NAME, "task.cancelled", { taskId: task.id, phase: "planning" });
        if (task.discord_thread_id) {
          await postReply(task.discord_thread_id, "Cancelled — stopped during planning, no changes made.");
        }
        return;
      }
      planningSessionId = planningResult.planningSessionId;

      if (task.discord_thread_id) {
        await postReply(task.discord_thread_id, "Approved — implementing now.");
      }
    } else {
      log("info", "resuming implementation after crash recovery", { worker: WORKER_NAME, taskId: task.id });
      if (task.discord_thread_id) {
        await postReply(task.discord_thread_id, "Recovered after an interruption — resuming implementation.");
      }
    }

    await setTaskStatus(task.id, "implementing");
    const implResult = await runImplementationPhase(task, planningSessionId);
    if (implResult.aborted) {
      log("info", "task cancelled", { worker: WORKER_NAME, taskId: task.id, phase: "implementation" });
      await setTaskStatus(task.id, "cancelled");
      await appendJournal(task.repo, WORKER_NAME, "task.cancelled", { taskId: task.id, phase: "implementation" });
      if (task.discord_thread_id) {
        await postReply(task.discord_thread_id, "Cancelled — stopped during implementation. Any local changes are being discarded (no PR opened).");
      }
      return;
    }
    const summary = implResult.summary.split("PR_READY:")[1]?.trim() ?? implResult.summary;

    // Guard against the rare split-brain case: this heartbeat went stale
    // during a network partition (not an actual crash), someone else
    // reclaimed the row, and connectivity has since come back — never open
    // a duplicate PR in that window. See docs/adr/0016.
    if (!(await stillHoldsLease(task.id, leaseId))) {
      log("warn", "lease lost before push — another claimant has taken over, aborting silently", { taskId: task.id });
      return;
    }

    const prUrl = await pushAndOpenPr(
      `/workspace/worktrees/${task.id}`,
      branch,
      task.description,
      summary,
    );

    await setTaskStatus(task.id, "done", { pr_url: prUrl });
    await appendJournal(task.repo, WORKER_NAME, "task.done", { taskId: task.id, prUrl });
    if (task.discord_thread_id) {
      await postReply(task.discord_thread_id, `Done. ${summary}\n\nPR: ${prUrl}`);
    }
  } catch (err) {
    if (err instanceof TransientError && (await requeueTransient(task.id, String(err), MAX_TASK_RETRIES))) {
      requeued = true;
      log("warn", "task requeued after transient failure", { worker: WORKER_NAME, taskId: task.id, error: String(err) });
      await appendJournal(task.repo, WORKER_NAME, "task.requeued", { taskId: task.id, error: String(err) });
      if (task.discord_thread_id) {
        await postReply(task.discord_thread_id, `Hit a transient error — automatically retrying: ${String(err)}`);
      }
      return;
    }
    await setTaskStatus(task.id, "failed");
    await appendJournal(task.repo, WORKER_NAME, "task.failed", {
      taskId: task.id,
      error: String(err),
    });
    if (task.discord_thread_id) {
      await postReply(task.discord_thread_id, `Task failed: ${String(err)}`);
    }
    log("error", "task failed", { worker: WORKER_NAME, taskId: task.id, error: String(err) });
  } finally {
    clearInterval(heartbeat);
    // A successful transient requeue deliberately keeps the worktree — the
    // next claim (by this same worker, since only it polls this repo)
    // reuses it. Otherwise, only remove it if this worker still holds the
    // lease (see the push-time check above for why that can fail).
    if (!requeued && (await stillHoldsLease(task.id, leaseId).catch(() => false))) {
      await removeWorktree(task.id, branch);
    }
  }
}

async function main(): Promise<void> {
  await configureGitAuth();
  await ensureRepoCloned(TARGET_REPO_URL as string);
  log("info", "worker ready", { worker: WORKER_NAME, targetRepo: TARGET_REPO });
  for (;;) {
    const task = await claimNextTask(TARGET_REPO as string, WORKER_NAME);
    if (task) {
      await handleTask(task);
    } else {
      await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
    }
  }
}

main().catch((err) => {
  log("error", "worker crashed", { worker: WORKER_NAME, error: String(err) });
  process.exit(1);
});
