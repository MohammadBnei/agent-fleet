import { test, expect } from "bun:test";
import { sessionBadge } from "./pages/TaskList";
import type { Task } from "./gen/agentfleet/v1/core_pb";

function task(over: Partial<Task>): Task {
  return {
    $typeName: "agentfleet.v1.Task",
    id: "t1", repo: "r", description: "d", status: "running", kind: "worker",
    retryCount: 0, awaitingHuman: false, liveState: "", ...over,
  } as Task;
}

// The screenshots that prompted this: a row showing "SCHEDULED DONE" (a pod
// phase contradicting a liveness state) and "CANCELLED TERMINATED" (the same
// fact twice). One badge, ranked, fixes both.
test("liveness outranks pod phase — never SCHEDULED next to DONE", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_SCHEDULED", liveState: "done" }));
  expect(b?.label).toBe("DONE");
});

test("a finished session with a torn-down pod reads once, from its status", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_TERMINATED", status: "cancelled", liveState: "" }));
  expect(b?.label).toBe("CANCELLED");
});

test("needing a human outranks everything, including a crash", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_CRASHED", liveState: "blocked", awaitingHuman: true }));
  expect(b?.label).toBe("NEEDS YOU");
});

test("a crash outranks ordinary progress", () => {
  expect(sessionBadge(task({ podPhase: "POD_PHASE_CRASHED", liveState: "working" }))?.label).toBe("CRASHED");
});

test("provisioning keeps its sub-step, which is the difference between progress and a wedge", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_PROVISIONING", podMessage: "cloning repo", liveState: "unknown" }));
  expect(b?.label).toBe("PROVISIONING: cloning repo");
});

test("an unapproved proposal is never confused with a queued task", () => {
  expect(sessionBadge(task({ status: "proposed", liveState: "" }))?.label).toBe("PROPOSED");
  expect(sessionBadge(task({ status: "pending", liveState: "" }))?.label).toBe("QUEUED");
});
