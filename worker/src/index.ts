import { claimNextTask, setTaskStatus, appendJournal } from "./db.js";
import {
  configureGitAuth,
  ensureRepoCloned,
  createWorktree,
  removeWorktree,
  pushAndOpenPr,
} from "./git.js";
import { postReply } from "./discord.js";
import { runPlanningPhase, runImplementationPhase } from "./planning.js";

const TARGET_REPO = process.env.TARGET_REPO;
const TARGET_REPO_URL = process.env.TARGET_REPO_URL;
const WORKER_NAME = process.env.WORKER_NAME ?? `${TARGET_REPO}-worker`;
const POLL_INTERVAL_MS = Number(process.env.POLL_INTERVAL_MS ?? 5000);

if (!TARGET_REPO || !TARGET_REPO_URL) {
  throw new Error("TARGET_REPO and TARGET_REPO_URL must be set");
}

async function handleTask(task: Awaited<ReturnType<typeof claimNextTask>>): Promise<void> {
  if (!task) return;
  console.log(`[${WORKER_NAME}] claimed task ${task.id}: ${task.description}`);
  await appendJournal(task.repo, WORKER_NAME, "task.claimed", { taskId: task.id });

  const { branch } = await createWorktree(task.id);
  try {
    await setTaskStatus(task.id, "planning");
    if (task.discord_thread_id) {
      await postReply(
        task.discord_thread_id,
        `Starting planning for **${task.description}**. Proposer and critic are debating the approach — reply here to join in, and say "approved" when you're happy with the plan.`,
      );
    }

    const { proposerSessionId } = await runPlanningPhase(task);

    if (task.discord_thread_id) {
      await postReply(task.discord_thread_id, "Approved — implementing now.");
    }

    const finalText = await runImplementationPhase(task, proposerSessionId);
    const summary = finalText.split("PR_READY:")[1]?.trim() ?? finalText;

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
    await setTaskStatus(task.id, "failed");
    await appendJournal(task.repo, WORKER_NAME, "task.failed", {
      taskId: task.id,
      error: String(err),
    });
    if (task.discord_thread_id) {
      await postReply(task.discord_thread_id, `Task failed: ${String(err)}`);
    }
    console.error(`[${WORKER_NAME}] task ${task.id} failed`, err);
  } finally {
    await removeWorktree(task.id, branch);
  }
}

async function main(): Promise<void> {
  await configureGitAuth();
  await ensureRepoCloned(TARGET_REPO_URL as string);
  console.log(`[${WORKER_NAME}] ready, polling for ${TARGET_REPO} tasks`);
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
  console.error("worker crashed", err);
  process.exit(1);
});
