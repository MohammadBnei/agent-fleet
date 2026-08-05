// Exercises reliability-findings.md #9: a sidecar blip right when the
// wrapper is reporting a failure must not cascade into an uncaught crash
// that leaves the task's status stuck until core's 10-min reclaim.
//
// main() takes its sidecar/runTask dependencies as parameters (defaulting
// to the real implementations) rather than being driven via
// mock.module() — Bun's mock.module is process-global, not per-file, and
// planning.test.ts independently needs the real ./planning.js and
// ./sidecarClient.js at its own top level; module-mocking either here
// would leak into that file's import and break it.
import { test, expect } from "bun:test";
import type { Task } from "./types.js";

process.env.TASK_ID = "task-1";
process.env.TARGET_REPO = "dream-analyst";
process.env.LEASE_ID = "lease-1";

const { main } = await import("./index.js");

test("a sidecar failure while reporting 'failed' status doesn't crash the process", async () => {
  const statusCalls: { status: string; fields?: unknown }[] = [];
  let failedStatusCallCount = 0;

  const fakeSidecar = {
    heartbeat: async () => {},
    appendJournal: async () => {},
    // "planning" succeeds (mirrors a sidecar that was fine until the very
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

  // The "planning" status write succeeded before the simulated failure.
  expect(statusCalls).toContainEqual({ status: "planning", fields: undefined });
  // The "failed" status write was attempted despite the sidecar throwing —
  // reliability-findings.md #9's guard wraps it, it doesn't get skipped.
  expect(failedStatusCallCount).toBeGreaterThan(0);
  // main() completed normally by containing the double-failure locally —
  // it never needed to escape to the top-level crash handler.
  expect(exitCode).toBe("not-called");
});
