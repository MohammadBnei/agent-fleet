import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { WorktreeViewSchema } from "../gen/agentfleet/v1/dashboard_pb";
import { isStaleWorktree, formatBytes, owner } from "./Worktrees";

const NOW = 1_800_000_000;
const OLD = BigInt(NOW - 3600); // well outside the mtime grace

function wt(liveState: string | undefined, mtimeUnix = OLD) {
  return create(WorktreeViewSchema, { sessionId: "t1", repo: "r", branch: "agent/t1", liveState, mtimeUnix });
}

// The statuses this used to enumerate ("failed", "cancelled",
// "failed_permanently") are gone with the enum in docs/adr/0048. What matters
// now is whether a pod is attached — and the case that regressed silently is
// the empty live state, which is what a STOPPED session reports and therefore
// the overwhelmingly common way a worktree becomes reclaimable.
test("a worktree whose session has no live pod is stale", () => {
  expect(isStaleWorktree(wt("done"), NOW)).toBe(true);
  expect(isStaleWorktree(wt(""), NOW)).toBe(true);
  // No session row at all — the orphaned case this view exists to surface.
  expect(isStaleWorktree(wt(undefined), NOW)).toBe(true);
});

// blocked and stalled both still have a live pod with an agent attached; they
// are waiting on a human, not finished. Reclaiming either deletes a working
// tree out from under a session someone is about to answer.
test("a worktree with a live pod is never stale, including one waiting on a human", () => {
  for (const state of ["working", "idle", "unknown", "blocked", "stalled"]) {
    expect(isStaleWorktree(wt(state), NOW)).toBe(false);
  }
});

// The pod-still-finishing guard: a pod can still be mid-push when its session
// already reads as having no live pod. Without this the sync would yank the
// checkout and cost the PR.
test("a recently touched worktree is spared even when its session has ended", () => {
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

  const live = owner(wt("working"));
  expect(live.orphan).toBe(false);
  expect(live.label).toBe("#t1 working");
  // Live sessions are called out in the blocked/attention colour, not dimmed.
  expect(live.cls).toBe("text-error");

  const finished = owner(wt("done"));
  expect(finished.orphan).toBe(false);
  expect(finished.cls).toBe("text-dim");
});
