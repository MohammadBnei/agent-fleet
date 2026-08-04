// Exercises git.ts's createWorktree idempotency (docs/adr/0016) against a
// real local git repo in a tmpdir — no mocking, no network. REPO_ROOT/
// WORKTREES_ROOT/BASE_BRANCH are read from process.env at module load time,
// so they must be set before the dynamic import below (same top-level-await
// ordering constraint as planning.test.ts's mock.module calls).
import { test, expect, afterAll } from "bun:test";
import { $ } from "bun";
import { mkdtemp, rm, writeFile, readdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const root = await mkdtemp(join(tmpdir(), "git-test-"));
const originPath = join(root, "origin");
const repoRoot = join(root, "repo");

// Bare-ish origin with one commit on main, plus a real clone standing in
// for the worker's own persistent checkout (git.ts's REPO_ROOT).
await $`git init -q -b main ${originPath}`;
await $`git -C ${originPath} config user.email test@test.com`;
await $`git -C ${originPath} config user.name test`;
await writeFile(join(originPath, "README.md"), "hello\n");
await $`git -C ${originPath} add -A`;
await $`git -C ${originPath} commit -q -m init`;
await $`git clone -q ${originPath} ${repoRoot}`;

process.env.REPO_ROOT = repoRoot;
process.env.WORKTREES_ROOT = join(root, "worktrees");
process.env.BASE_BRANCH = "main";

const { createWorktree, removeWorktree } = await import("./git.js");

afterAll(async () => {
  await rm(root, { recursive: true, force: true });
});

test("fresh claim creates a new worktree on a new branch from origin", async () => {
  const taskId = crypto.randomUUID();
  const { path, branch } = await createWorktree(taskId, false);

  expect(branch).toBe(`agent/${taskId}`);
  expect(await readdir(path)).toContain("README.md");
});

test("resume=true reuses an existing worktree as-is, diff intact", async () => {
  const taskId = crypto.randomUUID();
  const { path } = await createWorktree(taskId, false);
  await writeFile(join(path, "wip.txt"), "half-finished work\n");
  await $`git -C ${path} add -A`;
  await $`git -C ${path} -c user.email=test@test.com -c user.name=test commit -q -m wip`;

  const resumed = await createWorktree(taskId, true);

  expect(await readdir(resumed.path)).toContain("wip.txt");
});

test("resume=false wipes and recreates, discarding a dead attempt's partial edits", async () => {
  const taskId = crypto.randomUUID();
  const { path } = await createWorktree(taskId, false);
  await writeFile(join(path, "dirty.txt"), "uncommitted partial edit\n");

  const recreated = await createWorktree(taskId, false);

  expect(await readdir(recreated.path)).not.toContain("dirty.txt");
});

test("recreates from an existing branch when the worktree dir is gone but the branch survives", async () => {
  const taskId = crypto.randomUUID();
  const { path, branch } = await createWorktree(taskId, false);
  // Simulate a hard crash mid-cleanup: directory gone, branch still exists,
  // admin metadata left dangling — createWorktree's unconditional `worktree
  // prune` at the top must clear that before re-adding.
  await rm(path, { recursive: true, force: true });

  const recreated = await createWorktree(taskId, false);

  expect(recreated.branch).toBe(branch);
  expect(await readdir(recreated.path)).toContain("README.md");
});

test("removeWorktree cleans up both the directory and the branch", async () => {
  const taskId = crypto.randomUUID();
  const { branch } = await createWorktree(taskId, false);

  await removeWorktree(taskId, branch);

  const branches = await $`git -C ${repoRoot} branch --list ${branch}`.text();
  expect(branches.trim()).toBe("");
});
