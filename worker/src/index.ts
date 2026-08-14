// Single-shot entrypoint (docs/adr/0019): no poll loop, no claiming — the
// provisioner hands this pod exactly one task via env and the pod exits
// when it's done. Replaces the old claim-loop index.ts entirely; git
// clone/worktree setup and retry/crash-recovery now live in the provisioner
// (docs/adr/0019 point 2), not here.
import { runTask as defaultRunTask, TransientError } from "./session.js";
import * as defaultSidecar from "./sidecarClient.js";
import { log } from "./log.js";
import type { Task } from "./types.js";
import * as metrics from "./metrics.js";

type Sidecar = typeof defaultSidecar;
type RunTask = typeof defaultRunTask;
type ConfigureGitAuth = () => Promise<void>;

const TASK_ID = process.env.TASK_ID;
const TARGET_REPO = process.env.TARGET_REPO;
const LEASE_ID = process.env.LEASE_ID;
const HEARTBEAT_INTERVAL_MS = 30_000;
const WORKTREE_PATH = process.env.WORKTREE_PATH ?? "/workspace";

if (!TASK_ID || !TARGET_REPO || !LEASE_ID) {
  throw new Error("TASK_ID, TARGET_REPO, and LEASE_ID must be set");
}

// Task data (description, guidance, baseBranch, permissionMode, model) is now
// fetched from the database on startup instead of being passed via env vars.
// This ensures the worker always has fresh data from the source of truth, even
// on resume/restart. Only pod identity (TASK_ID, TARGET_REPO, LEASE_ID) comes
// from env vars.
async function buildTask(sidecar: Sidecar): Promise<Task> {
  const taskData = await sidecar.getTask();
  return {
    id: TASK_ID!,
    repo: TARGET_REPO!,
    leaseId: LEASE_ID!,
    description: taskData.description,
    guidance: taskData.guidance,
    baseBranch: taskData.baseBranch,
    permissionMode: taskData.permissionMode,
    model: taskData.model,
  };
}

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
// `gh pr create`, run once unconditionally before the session starts
// (reliability-findings.md #0) rather than lazily right before a push
// call the wrapper itself used to make — the agent now runs those
// commands itself via Bash, mid-session, so the auth has to already be in
// place whenever it decides to.
async function defaultConfigureGitAuth(): Promise<void> {
  if (!process.env.GH_TOKEN) return; // falls back to whatever ambient git auth is configured
  // The worktree was created by the provisioner's own container (a
  // different UID — worker runs non-root, required for Claude Code's
  // bypassPermissions mode, see worker/Dockerfile) — git's own
  // ownership-mismatch guard (CVE-2022-24765 mitigation) refuses every
  // command otherwise, "detected dubious ownership", regardless of the
  // worktree's actual file permissions. Confirmed live against a real
  // Longhorn-backed PVC: file perms alone (provisioner's umask 0) were not
  // sufficient, this config is a second, separate requirement.
  await run(["git", "config", "--global", "--add", "safe.directory", WORKTREE_PATH], WORKTREE_PATH);
  await run(["gh", "auth", "setup-git"], WORKTREE_PATH);
  const login = await run(["gh", "api", "user", "--jq", ".login"], WORKTREE_PATH);
  await run(["git", "config", "--global", "user.name", login], WORKTREE_PATH);
  await run(["git", "config", "--global", "user.email", `${login}@users.noreply.github.com`], WORKTREE_PATH);
}

// Bounded retry for the one sidecar call that's load-bearing at startup
// (see below) — a single transient blip (e.g. core mid-rollout) shouldn't
// permanently fail a task that would otherwise have succeeded a second
// later. Not used elsewhere: every other sidecar call in this file already
// treats failure as best-effort via reportStatus/.catch(...).
async function withRetry<T>(fn: () => Promise<T>, attempts = 3, delayMs = 1000): Promise<T> {
  let lastErr: unknown;
  for (let i = 0; i < attempts; i++) {
    try {
      return await fn();
    } catch (err) {
      lastErr = err;
      if (i < attempts - 1) await new Promise((resolve) => setTimeout(resolve, delayMs * 2 ** i));
    }
  }
  throw lastErr;
}

// Best-effort status write with its own guard — used everywhere a status
// report is the last thing standing between a real outcome and a task stuck
// at a stale status until core's 10-min heartbeat reclaim (reliability
// finding #9: a sidecar blip at exactly the wrong moment must not cascade
// into total silence).
async function reportStatus(sidecar: Sidecar, taskId: string, status: string, fields?: Parameters<Sidecar["setStatus"]>[1]): Promise<void> {
  try {
    await sidecar.setStatus(status, fields);
  } catch (err) {
    log("error", "failed to report status to sidecar", { taskId, status, error: String(err) });
  }
}

// sidecar/runTask/configureGitAuth default to the real implementations —
// the parameters exist so tests can substitute fakes without touching
// module resolution (index.test.ts and session.test.ts both need to
// control this same boundary independently; Bun's mock.module is
// process-global, not per-file, so module-mocking any of these here would
// leak into session.test.ts's own real import of ./session.js).
export async function main(
  sidecar: Sidecar = defaultSidecar,
  runTask: RunTask = defaultRunTask,
  configureGitAuth: ConfigureGitAuth = defaultConfigureGitAuth,
): Promise<void> {
  // Fetch fresh task data from the database via sidecar. This ensures we have
  // the latest description, guidance, baseBranch, permissionMode, and model
  // from the source of truth, not stale env vars.
  const task = await buildTask(sidecar);

  log("info", "task starting", { taskId: task.id, repo: task.repo });
  await sidecar
    .appendJournal(task.repo, "worker", "task.claimed", { taskId: task.id })
    .catch((err) => log("warn", "appendJournal(task.claimed) failed", { taskId: task.id, error: String(err) }));

  const heartbeat = setInterval(() => {
    sidecar.heartbeat(task.leaseId).catch((err) => log("warn", "heartbeat failed", { taskId: task.id, error: String(err) }));
  }, HEARTBEAT_INTERVAL_MS);

  try {
    await configureGitAuth();
    // Unlike every other status write in this function, this one runs
    // before the agent session even starts — a bare throw here previously
    // fell straight to the generic "failed" path below and killed the task
    // in well under a second on any transient core blip (observed live:
    // "produced zero addresses" during a core rollout). Retried a few
    // times, then reframed as TransientError so a blip that outlasts the
    // retries still gets core's normal reclaim-and-redispatch treatment
    // instead of being marked (permanently, after enough retries) failed.
    try {
      await withRetry(() => sidecar.setStatus("running"));
    } catch (err) {
      throw new TransientError(`sidecar.setStatus("running") failed after retries: ${err}`);
    }
    // runTask only ever returns normally via a human-initiated Stop — no
    // automated completion detection anywhere (no PR_READY: text match, no
    // PR-existence check). The human was watching the live dashboard
    // transcript before deciding to stop it, so there's nothing here to
    // infer; "cancelled" is reused as the single "ended via Stop" terminal
    // status rather than adding a new enum value for it.
    const result = await runTask(task);
    log("info", "task ended", { taskId: task.id, aborted: result.aborted });
    await sidecar.setStatus("cancelled", { notes: result.summary });
    await sidecar
      .appendJournal(task.repo, "worker", "task.stopped", { taskId: task.id })
      .catch((err) => log("warn", "appendJournal(task.stopped) failed", { taskId: task.id, error: String(err) }));
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
      // A non-zero exit is the point, independent of the journal/status
      // writes above succeeding or not: it's what makes the k8s Job phase
      // Failed instead of Succeeded, which is what the provisioner's
      // reconcile loop (and its fast-path crash report to core) keys off
      // of — the one signal that survives even when every sidecar call
      // above failed too (see docs/reliability-backlog, "swallowed task
      // failure" finding).
      process.exitCode = 1;
      return;
    }
    // SDK abort errors (from interrupt) are expected and already handled in
    // session.ts catch block. Only log as error if genuinely unexpected.
    const errStr = String(err);
    const isAbortError = errStr.includes("Request was aborted") || errStr.includes("AbortError");
    if (!isAbortError) {
      await reportStatus(sidecar, task.id, "failed", { lastError: errStr });
      await sidecar
        .appendJournal(task.repo, "worker", "task.failed", { taskId: task.id, error: errStr })
        .catch((journalErr) => log("warn", "appendJournal(task.failed) failed", { taskId: task.id, error: String(journalErr) }));
      log("error", "task failed", { taskId: task.id, error: errStr });
      process.exitCode = 1;
    }
  } finally {
    clearInterval(heartbeat);
  }
}

// import.meta.main is false when this module is imported (e.g. by
// index.test.ts) rather than run directly — keeps main() exported and
// independently invokable for tests, without a top-level side effect.
if (import.meta.main) {
  // Start metrics HTTP server on port 9092 (worker container)
  // ponytail: worker metrics on 9092, sidecar on 9091. Prometheus scrapes pod, not container.
  const metricsServer = Bun.serve({
    port: 9092,
    fetch(req) {
      const url = new URL(req.url);
      if (url.pathname === "/metrics") {
        return new Response(metrics.renderMetrics(), {
          headers: { "Content-Type": "text/plain; version=0.0.4" },
        });
      }
      return new Response("Not Found", { status: 404 });
    },
  });
  log("info", "worker metrics listening", { port: 9092 });

  main().catch(async (err) => {
    log("error", "worker crashed", { taskId: TASK_ID, error: String(err) });
    // Last-resort attempt: everything above already guards its own status
    // writes, so reaching here means something unforeseen slipped past
    // those guards — still worth one more try before the pod exits and
    // the task sits stuck until core's 10-min reclaim.
    await reportStatus(defaultSidecar, TASK_ID!, "failed", { lastError: String(err) });
    metricsServer.stop();
    process.exit(1);
  });
}
