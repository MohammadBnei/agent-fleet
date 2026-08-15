import { test, expect } from "bun:test";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import { compareSessions } from "./pages/SessionList";

function session(id: string, over: Partial<Session> = {}): Session {
  return {
    $typeName: "agentfleet.v1.Session",
    id,
    repo: "agent-fleet",
    description: id,
    pendingDecisions: 0,
    ...over,
  } as Session;
}

// date (default): most recently active first. proto Session has no createdAt,
// so this sorts on lastActiveAt, with a 0 fallback for one that never became
// active — which must therefore sink to the bottom, not float to the top.
test("date sort is most-recent-first, never-active last", () => {
  const older = session("older", { lastActiveAt: "2026-08-01T00:00:00Z" });
  const newer = session("newer", { lastActiveAt: "2026-08-10T00:00:00Z" });
  const never = session("never");
  const sorted = [never, older, newer].sort((a, b) => compareSessions(a, b, "date"));
  expect(sorted.map((s) => s.id)).toEqual(["newer", "older", "never"]);
});

// title sort uses sessionLabel, which falls back to the description here.
test("title sort is alphabetical by label", () => {
  const sorted = [session("c", { description: "charlie" }), session("a", { description: "alpha" }), session("b", { description: "bravo" })]
    .sort((a, b) => compareSessions(a, b, "title"));
  expect(sorted.map((s) => s.description)).toEqual(["alpha", "bravo", "charlie"]);
});

test("repo sort groups by repo", () => {
  const sorted = [session("z", { repo: "zeta" }), session("a", { repo: "alpha" })]
    .sort((a, b) => compareSessions(a, b, "repo"));
  expect(sorted.map((s) => s.repo)).toEqual(["alpha", "zeta"]);
});
