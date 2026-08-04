import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdir, rm } from "node:fs/promises";

const run = promisify(execFile);

const REPO_ROOT = process.env.REPO_ROOT ?? "/workspace/repo";
const WORKTREES_ROOT = process.env.WORKTREES_ROOT ?? "/workspace/worktrees";

async function git(args: string[], cwd = REPO_ROOT): Promise<string> {
  const { stdout } = await run("git", args, { cwd });
  return stdout.trim();
}

// One bot-account PAT (GH_TOKEN, Infisical) covers everything: `gh auth
// setup-git` wires it into git's credential helper for HTTPS push/clone, and
// gh CLI itself already reads GH_TOKEN for the REST/GraphQL calls behind
// `gh pr create`. Needs the token in the env before this runs.
export async function configureGitAuth(): Promise<void> {
  if (!process.env.GH_TOKEN) return; // falls back to whatever ambient git auth is configured
  await run("gh", ["auth", "setup-git"]);

  // git commit has no identity by default in a fresh container — derive it
  // from the actual authenticated bot account rather than hardcoding a
  // guess, so it stays correct if the bot account ever changes. users.noreply
  // works regardless of whether the account's real email is public.
  const login = (await run("gh", ["api", "user", "--jq", ".login"])).stdout.trim();
  await run("git", ["config", "--global", "user.name", login]);
  await run("git", ["config", "--global", "user.email", `${login}@users.noreply.github.com`]);
}

export async function ensureRepoCloned(repoUrl: string): Promise<void> {
  try {
    await run("git", ["rev-parse", "--is-inside-work-tree"], { cwd: REPO_ROOT });
    await git(["fetch", "origin"]);
  } catch {
    await mkdir(REPO_ROOT, { recursive: true });
    await run("git", ["clone", repoUrl, REPO_ROOT]);
  }
}

// Some target repos don't develop off `main` (e.g. vos-monolith's default
// branch is `dev` — `main` only receives prod tag bumps, see
// gitops/apps/registry.yaml's comment) — configurable per worker deployment.
const BASE_BRANCH = process.env.BASE_BRANCH ?? "main";

// `resume` is true only when the caller is genuinely resuming a
// crash-reclaimed implementation-phase task (see docs/adr/0016) — every
// other path (fresh claim, reclaimed-at-planning/claimed) wipes and
// recreates so a dead attempt's partial edits never leak into a new one.
export async function createWorktree(taskId: string, resume: boolean): Promise<{ path: string; branch: string }> {
  const branch = `agent/${taskId}`;
  const path = `${WORKTREES_ROOT}/${taskId}`;
  await mkdir(WORKTREES_ROOT, { recursive: true });

  if (resume) {
    try {
      await run("git", ["rev-parse", "--is-inside-work-tree"], { cwd: path });
      const { stdout: head } = await run("git", ["rev-parse", "--abbrev-ref", "HEAD"], { cwd: path });
      if (head.trim() === branch) return { path, branch }; // genuine resume — reuse as-is, diff intact
    } catch {
      // no valid worktree at path — fall through to (re)create
    }
  } else {
    await rm(path, { recursive: true, force: true });
  }

  // Clear stale admin metadata (a killed git process, or the rm above)
  // immediately before adding — must run *after* the rm, not before, or a
  // dir we just deleted ourselves still looks "registered" to git.
  await git(["worktree", "prune"]).catch(() => {});

  const branchExists = (await git(["branch", "--list", branch])).length > 0;
  await git(branchExists
    ? ["worktree", "add", path, branch]
    : ["worktree", "add", "-b", branch, path, `origin/${BASE_BRANCH}`]);
  return { path, branch };
}

export async function removeWorktree(taskId: string, branch: string): Promise<void> {
  const path = `${WORKTREES_ROOT}/${taskId}`;
  try {
    await git(["worktree", "remove", "--force", path]);
  } catch {
    await rm(path, { recursive: true, force: true });
  }
  await git(["branch", "-D", branch]).catch(() => {});
}

// Pushes the branch and opens a PR — both authenticated via GH_TOKEN
// (configureGitAuth wired it into git's credential helper; gh CLI reads it
// directly for the PR-create API call).
export async function pushAndOpenPr(
  worktreePath: string,
  branch: string,
  title: string,
  body: string,
): Promise<string> {
  await run("git", ["push", "-u", "origin", branch], { cwd: worktreePath });
  const { stdout } = await run(
    "gh",
    ["pr", "create", "--title", title, "--body", body, "--head", branch, "--base", BASE_BRANCH],
    { cwd: worktreePath },
  );
  const match = stdout.match(/https:\/\/github\.com\/\S+/);
  if (!match) throw new Error(`gh pr create did not return a URL: ${stdout}`);
  return match[0];
}
