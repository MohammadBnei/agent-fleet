import { test, expect, beforeEach } from "bun:test";
import { mkdtempSync, writeFileSync, chmodSync, mkdirSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { FLEET_TRAILER, gitHooksEnv } from "./gitHooks.js";

// Runs real git against worker/git-hooks, the way an agent's `git commit` does
// inside a pod: core.hooksPath comes from gitHooksEnv, nothing else.
const HOOKS = join(import.meta.dir, "..", "git-hooks");
let repo: string;

function git(args: string[], extra: Record<string, string> = {}) {
  const base: Record<string, string> = {
    PATH: process.env.PATH ?? "",
    HOME: repo,
    GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@t", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@t",
    FLEET_GIT_HOOKS: HOOKS,
    ...extra,
  };
  const p = Bun.spawnSync(["git", ...args], { cwd: repo, env: { ...base, ...hooksFor(base) } });
  if (p.exitCode !== 0) throw new Error(p.stderr.toString());
  return p.stdout.toString();
}
// gitHooksEnv reads FLEET_GIT_HOOKS at import time, so the path is set here.
function hooksFor(env: Record<string, string>) {
  const out = gitHooksEnv(env);
  out[`GIT_CONFIG_VALUE_${Number(env.GIT_CONFIG_COUNT) || 0}`] = HOOKS;
  return out;
}
const msg = "fix: thing\n\nbody\n\nCo-Authored-By: Claude Opus 5 <noreply@anthropic.com>\nco-authored-by: Someone <s@x>\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nClaude-Session: https://claude.ai/code/x\n";
const lastMessage = () => git(["log", "-1", "--format=%B"]);

beforeEach(() => {
  repo = mkdtempSync(join(tmpdir(), "hooks-"));
  git(["init", "-q"]);
  writeFileSync(join(repo, "f"), "x");
  git(["add", "f"]);
});

test("repo session: every attribution line replaced by exactly the fleet trailer", () => {
  git(["commit", "-q", "-m", msg]);
  const out = lastMessage();
  expect(out).toContain("fix: thing");
  expect(out).toContain("body");
  expect(out.match(/co-authored-by/gi)?.length).toBe(1);
  expect(out).toContain(FLEET_TRAILER);
  expect(out).not.toMatch(/generated with|claude-session|anthropic/i);
});

test("definition pod: no trailer at all", () => {
  git(["commit", "-q", "-m", msg], { FLEET_DEFINITION: "dev-notes" });
  expect(lastMessage()).not.toMatch(/co-authored-by|generated with|claude-session/i);
});

test("the repo's own hooks still run, .git/hooks and a local core.hooksPath (husky)", () => {
  const ran = join(repo, "ran");
  mkdirSync(join(repo, ".git", "hooks"), { recursive: true });
  writeFileSync(join(repo, ".git", "hooks", "pre-commit"), `#!/bin/sh\necho pre-commit >> "${ran}"\n`);
  chmodSync(join(repo, ".git", "hooks", "pre-commit"), 0o755);
  git(["commit", "-q", "-m", "one"]);
  expect(readFileSync(ran, "utf8")).toBe("pre-commit\n");

  mkdirSync(join(repo, ".husky"));
  writeFileSync(join(repo, ".husky", "commit-msg"), `#!/bin/sh\necho husky >> "${ran}"\n`);
  chmodSync(join(repo, ".husky", "commit-msg"), 0o755);
  git(["config", "--local", "core.hooksPath", ".husky"]);
  writeFileSync(join(repo, "f"), "y");
  git(["commit", "-qam", "two"]);
  expect(readFileSync(ran, "utf8")).toBe("pre-commit\nhusky\n");
  expect(lastMessage()).toContain(FLEET_TRAILER);
});

test("a failing repo hook still fails the commit", () => {
  mkdirSync(join(repo, ".git", "hooks"), { recursive: true });
  writeFileSync(join(repo, ".git", "hooks", "commit-msg"), "#!/bin/sh\nexit 1\n");
  chmodSync(join(repo, ".git", "hooks", "commit-msg"), 0o755);
  expect(() => git(["commit", "-q", "-m", "x"])).toThrow();
});

test("gitHooksEnv appends after the provisioner's GIT_CONFIG_* instead of replacing it", () => {
  expect(gitHooksEnv({})).toMatchObject({ GIT_CONFIG_COUNT: "1", GIT_CONFIG_KEY_0: "core.hooksPath" });
  expect(gitHooksEnv({ GIT_CONFIG_COUNT: "1" })).toMatchObject({ GIT_CONFIG_COUNT: "2", GIT_CONFIG_KEY_1: "core.hooksPath" });
});
