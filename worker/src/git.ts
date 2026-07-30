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

export async function createWorktree(taskId: string): Promise<{ path: string; branch: string }> {
  const branch = `agent/${taskId}`;
  const path = `${WORKTREES_ROOT}/${taskId}`;
  await mkdir(WORKTREES_ROOT, { recursive: true });
  await git(["worktree", "add", "-b", branch, path, `origin/${BASE_BRANCH}`]);
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

// Pushes the branch and opens a PR via the `gh` CLI (already authenticated
// through GIT_PAT / gh's GITHUB_TOKEN env var — see the worker Dockerfile).
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
