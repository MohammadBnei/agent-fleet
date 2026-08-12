import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { WorktreeViewSchema } from "../gen/agentfleet/v1/dashboard_pb";
import { isStaleWorktree, formatBytes, owner } from "./Worktrees";

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

test("byte sizes round to a unit a human can compare at a glance", () => {
  expect(formatBytes(0)).toBe("—");
  expect(formatBytes(512)).toBe("512 B");
  expect(formatBytes(2048)).toBe("2 KB");
  expect(formatBytes(641728512)).toBe("612 MB");
  expect(formatBytes(1181116006)).toBe("1.1 GB");
  // bigint is what the wire actually carries.
  expect(formatBytes(1181116006n)).toBe("1.1 GB");
});

// This decides whether a row reads as an orphan and whether it links to a live
// session. Mislabelling a running worktree is how someone deletes work in
// progress, so both directions are pinned.
test("owner distinguishes an orphan, a live session and a finished one", () => {
  const orphan = owner(wt(undefined));
  expect(orphan.orphan).toBe(true);
  expect(orphan.label).toBe("orphan · no session");

  const live = owner(wt("running"));
  expect(live.orphan).toBe(false);
  expect(live.label).toBe("#t1 running");
  // Live sessions are called out in the blocked/attention colour, not dimmed.
  expect(live.cls).toBe("text-error");

  const finished = owner(wt("done"));
  expect(finished.orphan).toBe(false);
  expect(finished.cls).toBe("text-dim");
});
