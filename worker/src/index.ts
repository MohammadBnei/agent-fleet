// Single-shot entrypoint: no poll loop, no claiming, and as of
// docs/adr/0048 no status reporting and no heartbeat either. The provisioner
// hands this pod one session via env; the pod runs it and exits.
//
// What used to be here and is not any more:
//
//   - a 30s heartbeat timer, which only ever proved a timer was still firing
//     and went silent for the same reason the pod had stopped working.
//     Liveness reconciles against Kubernetes now.
//   - setStatus("running")/("cancelled")/("failed"), because a polymorphic
//     session has no completion this process is in a position to report. It
//     ends when a human stops it or when something throws, and pod_phase
//     already says which.
//   - withRetry and TransientError, which existed to hand a blip back to a
//     reclaim loop that no longer exists.
//
// What remains is: set up git auth, run the session, exit non-zero if it
// threw. The exit code is load-bearing — it is what makes the Job phase
// Failed rather than Succeeded, and the provisioner reports both.
import { runSession as defaultRunSession } from "./session.js";
import * as defaultSidecar from "./sidecarClient.js";
import { log } from "./log.js";

type Sidecar = typeof defaultSidecar;
type RunSession = typeof defaultRunSession;
type ConfigureGitAuth = () => Promise<void>;

const SESSION_ID = process.env.SESSION_ID;
const TARGET_REPO = process.env.TARGET_REPO;
const WORKTREE_PATH = process.env.WORKTREE_PATH ?? "/workspace";

if (!SESSION_ID || !TARGET_REPO) {
  throw new Error("SESSION_ID and TARGET_REPO must be set");
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

// The provisioner's own `gh auth setup-git` configures ITS OWN container's
// git credential helper — worker and provisioner are separate pods sharing
// only a volume, not $HOME. This pod needs its own auth to `git push`/
// `gh pr create`, run once unconditionally before the session starts rather
// than lazily before a push: the agent runs those commands itself, whenever
// it decides to.
//
// `exec`/`sleep` are injected for the same reason sidecar/runSession are on
// main() below — Bun's mock.module is process-global, so a test cannot mock
// Bun.spawn here without leaking into session.test.ts.
export async function defaultConfigureGitAuth(
  exec: (cmd: string[], cwd: string) => Promise<string> = run,
  sleep: (ms: number) => Promise<void> = Bun.sleep,
): Promise<void> {
  if (!process.env.GH_TOKEN) return; // falls back to ambient git auth
  // The session directory was created by the provisioner's container (a
  // different UID — worker runs non-root, required for Claude Code's
  // bypassPermissions mode). Git's ownership-mismatch guard
  // (CVE-2022-24765 mitigation) refuses every command otherwise, "detected
  // dubious ownership", regardless of actual file permissions. Confirmed
  // live: file perms alone (provisioner's umask 0) were not sufficient.
  await exec(["git", "config", "--global", "--add", "safe.directory", WORKTREE_PATH], WORKTREE_PATH);
  await exec(["gh", "auth", "setup-git"], WORKTREE_PATH);
  // The only call in this function that leaves the pod — the other four are
  // local config. It is also, therefore, the only one that can fail for a
  // reason that will have cleared by the time anyone looks: GitHub answered
  // 503 "No server is currently available to service your request" twice on
  // 2026-08-17, one second into a session, and with BackoffLimit 0 that
  // permanently burned the session over a blip. withRetry was deleted with
  // the reclaim loop it fed (docs/adr/0048) and nothing replaced it here.
  //
  // Still throws once the attempts run out: a real auth failure — revoked
  // token, wrong scopes — should stop the session at start, not an hour
  // later at the agent's first `git commit`.
  let login = "";
  for (let attempt = 1; ; attempt++) {
    try {
      login = await exec(["gh", "api", "user", "--jq", ".login"], WORKTREE_PATH);
      break;
    } catch (err) {
      if (attempt === 3) throw err;
      log("warn", "gh api user failed, retrying", { sessionId: SESSION_ID, attempt, error: String(err) });
      await sleep(attempt * 2000);
    }
  }
  await exec(["git", "config", "--global", "user.name", login], WORKTREE_PATH);
  await exec(["git", "config", "--global", "user.email", `${login}@users.noreply.github.com`], WORKTREE_PATH);
}

// sidecar/runSession/configureGitAuth default to the real implementations —
// the parameters exist so tests can substitute fakes without touching module
// resolution. Bun's mock.module is process-global rather than per-file, so
// module-mocking any of these here would leak into session.test.ts's own
// real import of ./session.js.
export async function main(
  sidecar: Sidecar = defaultSidecar,
  runSession: RunSession = defaultRunSession,
  configureGitAuth: ConfigureGitAuth = defaultConfigureGitAuth,
): Promise<void> {
  log("info", "session starting", { sessionId: SESSION_ID, repo: TARGET_REPO });
  await sidecar
    .appendJournal(TARGET_REPO!, "worker", "session.started", { sessionId: SESSION_ID })
    .catch((err) => log("warn", "appendJournal(session.started) failed", { sessionId: SESSION_ID, error: String(err) }));

  try {
    await configureGitAuth();
    // runSession returns only via a human-initiated Stop. There is no
    // automated completion detection anywhere — no PR_READY match, no
    // PR-existence check — because a session may end in a PR, an
    // explanation, or a dead end, and nothing here can tell those apart.
    const result = await runSession();
    log("info", "session ended", { sessionId: SESSION_ID, aborted: result.aborted });
    await sidecar
      .appendJournal(TARGET_REPO!, "worker", "session.stopped", { sessionId: SESSION_ID, summary: result.summary })
      .catch((err) => log("warn", "appendJournal(session.stopped) failed", { sessionId: SESSION_ID, error: String(err) }));
  } catch (err) {
    // SDK abort errors (from interrupt) are expected and already handled in
    // session.ts. Only a genuinely unexpected error should fail the pod.
    const errStr = String(err);
    const isAbortError = errStr.includes("Request was aborted") || errStr.includes("AbortError");
    if (!isAbortError) {
      await sidecar
        .appendJournal(TARGET_REPO!, "worker", "session.failed", { sessionId: SESSION_ID, error: errStr })
        .catch((journalErr) => log("warn", "appendJournal(session.failed) failed", { sessionId: SESSION_ID, error: String(journalErr) }));
      log("error", "session failed", { sessionId: SESSION_ID, error: errStr });
      // The non-zero exit is the point, independent of whether the journal
      // write above succeeded: it is what makes the Job phase Failed rather
      // than Succeeded, which is the one signal that survives even when
      // every sidecar call failed too. The provisioner reports both phases
      // and core reconciles against them.
      process.exitCode = 1;
    }
  }
}

// import.meta.main is false when this module is imported (e.g. by
// index.test.ts) rather than run directly — keeps main() exported and
// independently invokable, without a top-level side effect.
if (import.meta.main) {
  main()
    .catch((err) => {
      log("error", "worker crashed", { sessionId: SESSION_ID, error: String(err) });
      process.exitCode = 1;
    })
    // process.exitCode only *requests* an exit code; the process still waits
    // for the event loop to drain. A single lingering handle — an SSE reader
    // mid-reconnect, an unsettled fetch — and the pod runs forever with the
    // right exit code and no exit. That is indistinguishable from a working
    // session to everything outside: the Job never reaches a terminal phase,
    // pod_phase stays RUNNING, and the slot is never released. Five of those
    // and the fleet stops accepting work.
    //
    // This process is single-shot by definition, so exiting explicitly here
    // costs nothing and removes the whole failure class.
    .finally(() => process.exit(process.exitCode ?? 0));
}
