import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { WorktreeViewSchema } from "../gen/agentfleet/v1/dashboard_pb";
import { isStaleWorktree } from "./Worktrees";

const NOW = 1_800_000_000;
const OLD = BigInt(NOW - 3600); // well outside the mtime grace

function wt(taskStatus: string | undefined, mtimeUnix = OLD) {
  return create(WorktreeViewSchema, { taskId: "t1", repo: "r", branch: "agent/t1", taskStatus, mtimeUnix });
}

test("finished and orphaned worktrees are stale", () => {
  for (const status of ["done", "failed", "cancelled", "failed_permanently"]) {
    expect(isStaleWorktree(wt(status), NOW)).toBe(true);
  }
  // No task row at all — the orphaned case the Worktrees view exists to surface.
  expect(isStaleWorktree(wt(undefined), NOW)).toBe(true);
});

test("live worktrees are never stale", () => {
  for (const status of ["pending", "claimed", "running"]) {
    expect(isStaleWorktree(wt(status), NOW)).toBe(false);
  }
});

// The pod-still-finishing guard: a worker sets its terminal status just
// before exiting, so a "done" task can still be mid-push. Without this the
// sync would yank the checkout and cost the PR.
test("a recently touched worktree is spared even when its task is done", () => {
  expect(isStaleWorktree(wt("done", BigInt(NOW - 5)), NOW)).toBe(false);
  expect(isStaleWorktree(wt("done", BigInt(NOW - 119)), NOW)).toBe(false);
  expect(isStaleWorktree(wt("done", BigInt(NOW - 121)), NOW)).toBe(true);
});
