// Single-shot entrypoint (docs/adr/0019): no poll loop, no claiming — the
// provisioner hands this pod exactly one task via env and the pod exits
// when it's done. Replaces the old claim-loop index.ts entirely; git
// clone/worktree setup and retry/crash-recovery now live in the provisioner
// (docs/adr/0019 point 2), not here.
import { runTask, TransientError } from "./planning.js";
import * as sidecar from "./sidecarClient.js";
import { log } from "./log.js";
import type { Task } from "./types.js";

const TASK_ID = process.env.TASK_ID;
const TARGET_REPO = process.env.TARGET_REPO;
const TASK_DESCRIPTION = process.env.TASK_DESCRIPTION;
const LEASE_ID = process.env.LEASE_ID;
// Some target repos don't develop off `main` (e.g. vos-monolith's default
// branch is `dev`) — the provisioner already knows this per-repo config
// (tasks.KnownRepos in core) and threads it through at pod creation, same
// as it does for the worktree's own base branch.
const BASE_BRANCH = process.env.BASE_BRANCH ?? "main";
const HEARTBEAT_INTERVAL_MS = 30_000;
const WORKTREE_PATH = process.env.WORKTREE_PATH ?? "/workspace";

if (!TASK_ID || !TARGET_REPO || !LEASE_ID) {
  throw new Error("TASK_ID, TARGET_REPO, and LEASE_ID must be set");
}

const task: Task = { id: TASK_ID, repo: TARGET_REPO, description: TASK_DESCRIPTION ?? "", leaseId: LEASE_ID };

async function run(cmd: string[], cwd: string): Promise<string> {
  const proc = Bun.spawn(cmd, { cwd, stdout: "pipe", stderr: "pipe" });
  const [stdout, exitCode] = await Promise.all([new Response(proc.stdout).text(), proc.exited]);
  if (exitCode !== 0) {
    const stderr = await new Response(proc.stderr).text();
    throw new Error(`${cmd.join(" ")} failed: ${stderr}`);
  }
  return stdout.trim();
}

// The provisioner's own `gh auth setup-git` (docs/adr/0019 point 2) only
// configures ITS OWN container's git credential helper — worker and
// provisioner are separate pods/containers sharing only the /workspace
// PVC mount, not $HOME. This pod still needs its own auth to `git push`/
// `gh pr create` (ported from the old worker/src/git.ts's
// configureGitAuth, deleted when clone/worktree setup moved out but this
// half of it was still needed here).
async function configureGitAuth(): Promise<void> {
  if (!process.env.GH_TOKEN) return; // falls back to whatever ambient git auth is configured
  await run(["gh", "auth", "setup-git"], WORKTREE_PATH);
  const login = await run(["gh", "api", "user", "--jq", ".login"], WORKTREE_PATH);
  await run(["git", "config", "--global", "user.name", login], WORKTREE_PATH);
  await run(["git", "config", "--global", "user.email", `${login}@users.noreply.github.com`], WORKTREE_PATH);
}

async function pushAndOpenPr(branch: string, title: string, body: string): Promise<string> {
  await configureGitAuth();
  await run(["git", "push", "-u", "origin", branch], WORKTREE_PATH);
  const stdout = await run(["gh", "pr", "create", "--title", title, "--body", body, "--head", branch, "--base", BASE_BRANCH], WORKTREE_PATH);
  const match = stdout.match(/https:\/\/github\.com\/\S+/);
  if (!match) throw new Error(`gh pr create did not return a URL: ${stdout}`);
  return match[0];
}

async function main(): Promise<void> {
  log("info", "task starting", { taskId: task.id, repo: task.repo });
  await sidecar.appendJournal(task.repo, "worker", "task.claimed", { taskId: task.id }).catch(() => {});

  const heartbeat = setInterval(() => {
    sidecar.heartbeat(task.leaseId).catch(() => {});
  }, HEARTBEAT_INTERVAL_MS);

  try {
    await sidecar.setStatus("planning");
    const result = await runTask(task);

    if (result.aborted) {
      log("info", "task cancelled", { taskId: task.id });
      await sidecar.setStatus("cancelled");
      await sidecar.appendJournal(task.repo, "worker", "task.cancelled", { taskId: task.id }).catch(() => {});
      return;
    }

    const summary = result.summary.split("PR_READY:")[1]?.trim() ?? result.summary;

    // Guard against the rare split-brain case: this pod's heartbeat went
    // stale during a network partition (not an actual crash), core
    // reclaimed and dispatched a fresh pod, and connectivity has since come
    // back — never open a duplicate PR in that window. See docs/adr/0016.
    if (!(await sidecar.stillHoldsLease(task.leaseId))) {
      log("warn", "lease lost before push — another claimant has taken over, aborting silently", { taskId: task.id });
      return;
    }

    const branch = `agent/${task.id}`;
    const prUrl = await pushAndOpenPr(branch, task.description, summary);

    await sidecar.setStatus("done", { prUrl });
    await sidecar.appendJournal(task.repo, "worker", "task.done", { taskId: task.id, prUrl }).catch(() => {});
    log("info", "task done", { taskId: task.id, prUrl });
  } catch (err) {
    if (err instanceof TransientError) {
      // No requeue-and-retry here — that's core's job now (a stale
      // heartbeat past ADR-0016's window makes this task eligible for
      // ClaimNextTask's reclaim, which dispatches a fresh pod). This pod
      // just fails fast and exits; TransientError only changes the log
      // framing, not the outcome, since single-shot pods don't loop.
      log("warn", "task hit a transient error, leaving it for core to reclaim", { taskId: task.id, error: String(err) });
      await sidecar.appendJournal(task.repo, "worker", "task.transient_error", { taskId: task.id, error: String(err) }).catch(() => {});
      return;
    }
    await sidecar.setStatus("failed", { lastError: String(err) });
    await sidecar.appendJournal(task.repo, "worker", "task.failed", { taskId: task.id, error: String(err) }).catch(() => {});
    log("error", "task failed", { taskId: task.id, error: String(err) });
  } finally {
    clearInterval(heartbeat);
  }
}

main().catch((err) => {
  log("error", "worker crashed", { taskId: task.id, error: String(err) });
  process.exit(1);
});
