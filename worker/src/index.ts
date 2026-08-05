// Single-shot entrypoint (docs/adr/0019): no poll loop, no claiming — the
// provisioner hands this pod exactly one task via env and the pod exits
// when it's done. Replaces the old claim-loop index.ts entirely; git
// clone/worktree setup and retry/crash-recovery now live in the provisioner
// (docs/adr/0019 point 2), not here.
import { runTask as defaultRunTask, TransientError } from "./planning.js";
import * as defaultSidecar from "./sidecarClient.js";
import { log } from "./log.js";
import type { Task } from "./types.js";

type Sidecar = typeof defaultSidecar;
type RunTask = typeof defaultRunTask;

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

// `gh pr create` has no --json/structured-output mode (checked: gh 2.96.0's
// --help lists none) — per its own docs, on success it prints only the PR
// URL, so the fix is to anchor on that shape (a real .../pull/<n> URL, the
// last non-empty stdout line) instead of grabbing the first github.com
// substring anywhere in stdout, which could match an unrelated URL.
const PR_URL_RE = /^https:\/\/github\.com\/[^/\s]+\/[^/\s]+\/pull\/\d+$/;

async function pushAndOpenPr(branch: string, title: string, body: string): Promise<string> {
  await configureGitAuth();
  await run(["git", "push", "-u", "origin", branch], WORKTREE_PATH);
  const stdout = await run(["gh", "pr", "create", "--title", title, "--body", body, "--head", branch, "--base", BASE_BRANCH], WORKTREE_PATH);
  const lastLine = stdout.trim().split("\n").pop()?.trim() ?? "";
  if (!PR_URL_RE.test(lastLine)) throw new Error(`gh pr create did not return a PR URL: ${stdout}`);
  return lastLine;
}

// Best-effort status write with its own guard — used everywhere a status
// report is the last thing standing between a real outcome and a task stuck
// at a stale status until core's 10-min heartbeat reclaim (reliability
// finding #9: a sidecar blip at exactly the wrong moment must not cascade
// into total silence).
async function reportStatus(sidecar: Sidecar, status: string, fields?: Parameters<Sidecar["setStatus"]>[1]): Promise<void> {
  try {
    await sidecar.setStatus(status, fields);
  } catch (err) {
    log("error", "failed to report status to sidecar", { taskId: task.id, status, error: String(err) });
  }
}

// sidecar/runTask default to the real implementations — the parameters
// exist so tests can substitute fakes without touching module resolution
// (index.test.ts and planning.test.ts both need to control this same
// boundary independently; Bun's mock.module is process-global, not
// per-file, so module-mocking either of these here would leak into
// planning.test.ts's own real import of ./planning.js).
export async function main(sidecar: Sidecar = defaultSidecar, runTask: RunTask = defaultRunTask): Promise<void> {
  log("info", "task starting", { taskId: task.id, repo: task.repo });
  await sidecar
    .appendJournal(task.repo, "worker", "task.claimed", { taskId: task.id })
    .catch((err) => log("warn", "appendJournal(task.claimed) failed", { taskId: task.id, error: String(err) }));

  const heartbeat = setInterval(() => {
    sidecar.heartbeat(task.leaseId).catch((err) => log("warn", "heartbeat failed", { taskId: task.id, error: String(err) }));
  }, HEARTBEAT_INTERVAL_MS);

  try {
    await sidecar.setStatus("planning");
    const result = await runTask(task);

    if (result.aborted) {
      log("info", "task cancelled", { taskId: task.id });
      await sidecar.setStatus("cancelled");
      await sidecar
        .appendJournal(task.repo, "worker", "task.cancelled", { taskId: task.id })
        .catch((err) => log("warn", "appendJournal(task.cancelled) failed", { taskId: task.id, error: String(err) }));
      return;
    }

    const summary = result.summary.split("PR_READY:")[1]?.trim() ?? result.summary;

    // Guard against the rare split-brain case: this pod's heartbeat went
    // stale during a network partition (not an actual crash), core
    // reclaimed and dispatched a fresh pod, and connectivity has since come
    // back — never open a duplicate PR in that window. See docs/adr/0016.
    // A blip in the check itself (sidecar unreachable) is treated the same
    // as "lease lost" — safer to skip a push than risk a duplicate PR.
    let holdsLease: boolean;
    try {
      holdsLease = await sidecar.stillHoldsLease(task.leaseId);
    } catch (err) {
      log("warn", "stillHoldsLease check failed — assuming lease lost, skipping push", { taskId: task.id, error: String(err) });
      holdsLease = false;
    }
    if (!holdsLease) {
      log("warn", "lease lost before push — another claimant has taken over, aborting silently", { taskId: task.id });
      return;
    }

    const branch = `agent/${task.id}`;
    const prUrl = await pushAndOpenPr(branch, task.description, summary);

    await sidecar.setStatus("done", { prUrl });
    await sidecar
      .appendJournal(task.repo, "worker", "task.done", { taskId: task.id, prUrl })
      .catch((err) => log("warn", "appendJournal(task.done) failed", { taskId: task.id, error: String(err) }));
    log("info", "task done", { taskId: task.id, prUrl });
  } catch (err) {
    if (err instanceof TransientError) {
      // No requeue-and-retry here — that's core's job now (a stale
      // heartbeat past ADR-0016's window makes this task eligible for
      // ClaimNextTask's reclaim, which dispatches a fresh pod). This pod
      // just fails fast and exits; TransientError only changes the log
      // framing, not the outcome, since single-shot pods don't loop.
      log("warn", "task hit a transient error, leaving it for core to reclaim", { taskId: task.id, error: String(err) });
      await sidecar
        .appendJournal(task.repo, "worker", "task.transient_error", { taskId: task.id, error: String(err) })
        .catch((journalErr) => log("warn", "appendJournal(task.transient_error) failed", { taskId: task.id, error: String(journalErr) }));
      return;
    }
    await reportStatus(sidecar, "failed", { lastError: String(err) });
    await sidecar
      .appendJournal(task.repo, "worker", "task.failed", { taskId: task.id, error: String(err) })
      .catch((journalErr) => log("warn", "appendJournal(task.failed) failed", { taskId: task.id, error: String(journalErr) }));
    log("error", "task failed", { taskId: task.id, error: String(err) });
  } finally {
    clearInterval(heartbeat);
  }
}

// import.meta.main is false when this module is imported (e.g. by
// index.test.ts) rather than run directly — keeps main() exported and
// independently invokable for tests, without a top-level side effect.
if (import.meta.main) {
  main().catch(async (err) => {
    log("error", "worker crashed", { taskId: task.id, error: String(err) });
    // Last-resort attempt: everything above already guards its own status
    // writes, so reaching here means something unforeseen slipped past
    // those guards — still worth one more try before the pod exits and
    // the task sits stuck until core's 10-min reclaim.
    await reportStatus(defaultSidecar, "failed", { lastError: String(err) });
    process.exit(1);
  });
}
