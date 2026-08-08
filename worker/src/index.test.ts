// Exercises reliability-findings.md #9: a sidecar blip right when the
// wrapper is reporting a failure must not cascade into an uncaught crash
// that leaves the task's status stuck until core's 10-min reclaim.
//
// main() takes its sidecar/runTask dependencies as parameters (defaulting
// to the real implementations) rather than being driven via
// mock.module() — Bun's mock.module is process-global, not per-file, and
// session.test.ts independently needs the real ./session.js and
// ./sidecarClient.js at its own top level; module-mocking either here
// would leak into that file's import and break it.
import { test, expect, afterEach } from "bun:test";
import type { Task } from "./types.js";

process.env.TASK_ID = "task-1";
process.env.TARGET_REPO = "dream-analyst";
process.env.LEASE_ID = "lease-1";

const { main } = await import("./index.js");

// main() now sets the real process.exitCode on a task failure (that's the
// point — see the new test below), which several tests in this file
// trigger on purpose for unrelated reasons. Without resetting it, that
// leaks past this file into bun test's own overall exit code. Reset to 0,
// not undefined — this Bun version (1.3.6) treats assigning `undefined`
// after a prior write as a no-op, so it wouldn't actually clear it.
afterEach(() => {
  process.exitCode = 0;
});

test("a sidecar failure while reporting 'failed' status doesn't crash the process", async () => {
  const statusCalls: { status: string; fields?: unknown }[] = [];
  let failedStatusCallCount = 0;

  const fakeSidecar = {
    heartbeat: async () => {},
    appendJournal: async () => {},
    getTask: async () => ({
      description: "test task",
      guidance: "",
      baseBranch: "main",
      permissionMode: "default",
      model: "claude-opus-4-8",
    }),
    // "running" succeeds (mirrors a sidecar that was fine until the very
    // end); "failed" — the status write index.ts's catch block makes
    // right as the task is going down — throws, simulating the blip
    // finding #9 describes.
    setStatus: async (status: string, fields?: unknown) => {
      if (status === "failed") {
        failedStatusCallCount++;
        throw new Error("sidecar unreachable");
      }
      statusCalls.push({ status, fields });
    },
    stillHoldsLease: async () => true,
    saveSessionId: async () => {},
    pushMessage: async () => {},
    streamHumanMessages: async () => {},
  };

  const fakeRunTask = async (_task: Task) => {
    throw new Error("simulated implementation failure");
  };

  let exitCode: number | "not-called" = "not-called";
  process.exit = ((code?: number) => {
    exitCode = code ?? 0;
    return undefined as never;
  }) as typeof process.exit;

  // Fakes satisfy the real module's shape structurally, not nominally —
  // `as never` sidesteps the exact-typing mismatch (e.g. mock functions
  // aren't wrapped in bun:test's `mock()`).
  await main(fakeSidecar as never, fakeRunTask as never);

  // The "running" status write succeeded before the simulated failure.
  expect(statusCalls).toContainEqual({ status: "running", fields: undefined });
  // The "failed" status write was attempted despite the sidecar throwing —
  // reliability-findings.md #9's guard wraps it, it doesn't get skipped.
  expect(failedStatusCallCount).toBeGreaterThan(0);
  // main() completed normally by containing the double-failure locally —
  // it never needed to escape to the top-level crash handler.
  expect(exitCode).toBe("not-called");
});

// Locks in the fix for the bug fleet-debug found: a real task failure was
// swallowed inside main()'s own catch block, so the process still exited 0
// — the provisioner's reconcile GC then saw the k8s Job as "Succeeded" and
// discarded it, even though the task had actually failed.
test("a real task failure sets a non-zero exit code even though main() handles it internally", async () => {
  const statusCalls: { status: string; fields?: unknown }[] = [];
  const fakeRunTask = async (_task: Task) => {
    throw new Error("simulated implementation failure");
  };

  await main(fakeSidecarRecording(statusCalls) as never, fakeRunTask as never);
  expect(process.exitCode).toBe(1);
});

function fakeSidecarRecording(statusCalls: { status: string; fields?: unknown }[]) {
  return {
    heartbeat: async () => {},
    appendJournal: async () => {},
    getTask: async () => ({
      description: "test task",
      guidance: "",
      baseBranch: "main",
      permissionMode: "default",
      model: "claude-opus-4-8",
    }),
    setStatus: async (status: string, fields?: unknown) => {
      statusCalls.push({ status, fields });
    },
    stillHoldsLease: async () => true,
    saveSessionId: async () => {},
    pushMessage: async () => {},
    streamHumanMessages: async () => {},
  };
}

// No automated completion detection anywhere (docs/adr supersession of the
// old PR_READY:-text-match/PR-existence-check design) — a session only
// ever ends via the human's own Stop, so main() reports one single
// terminal status regardless of what the agent's own summary text says or
// whether a PR happens to exist yet.
test("a session ending reports a single 'cancelled' status, with no PR-existence check", async () => {
  const statusCalls: { status: string; fields?: unknown }[] = [];
  const fakeRunTask = async (_task: Task) => ({ aborted: true, summary: "stopped mid-conversation" });
  const fakeConfigureGitAuth = async () => {};

  await main(fakeSidecarRecording(statusCalls) as never, fakeRunTask as never, fakeConfigureGitAuth);

  expect(statusCalls.some((c) => c.status === "done")).toBe(false);
  expect(statusCalls.some((c) => c.status === "failed")).toBe(false);
  const endedCall = statusCalls.find((c) => c.status === "cancelled");
  expect(endedCall).toBeDefined();
  expect((endedCall?.fields as { notes?: string } | undefined)?.notes).toBe("stopped mid-conversation");
});
