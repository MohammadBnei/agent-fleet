import { test, expect } from "bun:test";
import { sessionBadge } from "./pages/TaskList";
import type { Session } from "./gen/agentfleet/v1/core_pb";

function task(over: Partial<Session>): Session {
  return {
    $typeName: "agentfleet.v1.Session",
    id: "t1", repo: "r", description: "d",
    pendingDecisions: 0, liveState: "", ...over,
  } as Session;
}

// The screenshots that prompted this: a row showing "SCHEDULED DONE" (a pod
// phase contradicting a liveness state) and "CANCELLED TERMINATED" (the same
// fact twice). One badge, ranked, fixes both.
test("liveness outranks pod phase — never SCHEDULED next to DONE", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_SCHEDULED", liveState: "done" }));
  expect(b?.label).toBe("DONE");
});

// The five workflow statuses this used to render (PROPOSED, QUEUED, FAILED,
// FAILED (final), CANCELLED) went with the enum in docs/adr/0048. A stopped
// session is not a state of its own any more — it is simply a session with no
// live pod, which is the resting state of every session between messages, and
// badging it would put a permanent label on the common case.
test("a stopped session with no live pod carries no badge at all", () => {
  expect(sessionBadge(task({ podPhase: "POD_PHASE_TERMINATED", liveState: "" }))).toBeNull();
});

// What DOES survive teardown is the two things that are about the session
// rather than about a workflow position.
test("archived and swept are the states that outlive the pod", () => {
  expect(sessionBadge(task({ podPhase: "POD_PHASE_TERMINATED", archivedAt: "2026-08-01T00:00:00Z" }))?.label).toBe("ARCHIVED");
  expect(sessionBadge(task({ podPhase: "POD_PHASE_TERMINATED", sweptAt: "2026-08-01T00:00:00Z" }))?.label).toBe("SWEPT");
});

// A session that died with a reason must say so — with no `failed` status
// left, lastError is the only thing carrying it.
test("a torn-down session that recorded an error says so", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_TERMINATED", lastError: "pod never produced output" }));
  expect(b?.label).toBe("ERROR");
  expect(b?.title).toBe("pod never produced output");
});

test("needing a human outranks everything, including a crash", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_CRASHED", liveState: "blocked", pendingDecisions: 1 }));
  expect(b?.label).toBe("NEEDS YOU");
});

test("a crash outranks ordinary progress", () => {
  expect(sessionBadge(task({ podPhase: "POD_PHASE_CRASHED", liveState: "working" }))?.label).toBe("CRASHED");
});

test("provisioning keeps its sub-step, which is the difference between progress and a wedge", () => {
  const b = sessionBadge(task({ podPhase: "POD_PHASE_PROVISIONING", podMessage: "cloning repo", liveState: "unknown" }));
  expect(b?.label).toBe("PROVISIONING: cloning repo");
});

// This used to assert PROPOSED vs QUEUED. Neither exists: a proposal is a row
// in its own table with its own view (it has no pod path at all), and there is
// no queue for anything to be QUEUED in — a session's first message provisions
// its pod directly or is refused at the cap.
//
// What replaces it is the ordering that still matters: a live pod's state
// always beats a leftover fact about the session.
test("a live pod's state outranks archived/swept, which describe a session at rest", () => {
  expect(sessionBadge(task({ liveState: "working", archivedAt: "2026-08-01T00:00:00Z" }))?.label).toBe("WORKING");
  expect(sessionBadge(task({ liveState: "blocked", sweptAt: "2026-08-01T00:00:00Z" }))?.label).toBe("NEEDS YOU");
});
