import { test, expect } from "bun:test";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import { bucketSessions } from "./pages/SessionList";

// These tests used to be written against sessions.status ("proposed", "running",
// "done") and a `shipped` bucket. All three are gone in docs/adr/0048: a
// polymorphic session has no computable completion, so there is no status
// enum, and what a human actually sorts by is liveness plus whether anything
// is waiting on them. The property being guarded is unchanged.
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

// The reason this function is shared and tested at all: bucketSessions is the
// single membership split for BOTH form factors, and a session matching no
// bucket does not merely sort oddly — it vanishes from the list entirely,
// which is the one place a human is supposed to see it. `quiet` is therefore
// defined by exclusion rather than by its own predicate.
test("every session lands in exactly one bucket, whatever shape it is in", () => {
  const all = [
    session("blocked", { pendingDecisions: 2, liveState: "blocked" }),
    session("busy", { liveState: "working" }),
    session("done", { liveState: "done" }),
    session("stuck", { liveState: "stalled" }),
    session("crashed", { podPhase: "POD_PHASE_CRASHED" }),
    session("archived", { archivedAt: "2026-08-01T00:00:00Z" }),
    session("swept", { sweptAt: "2026-08-01T00:00:00Z" }),
    session("idle", { liveState: "idle" }),
    session("blank"), // no liveState, no phase — a session created and never messaged
    // The overlaps that used to double-render: independent filters meant a
    // swept-and-idle session appeared under both "idle" and "swept", and an
    // archived-and-stalled one under both "archived" and "stalled".
    session("swept-idle", { sweptAt: "2026-08-01T00:00:00Z", liveState: "idle" }),
    session("archived-stalled", { archivedAt: "2026-08-01T00:00:00Z", liveState: "stalled" }),
    session("archived-swept", { archivedAt: "2026-08-01T00:00:00Z", sweptAt: "2026-08-01T00:00:00Z" }),
  ];

  const b = bucketSessions(all, new Set());
  const placements = [
    ...b.needsYou,
    ...b.working,
    ...b.finished,
    ...b.stalled,
    ...b.archived,
    ...b.swept,
    ...b.quiet,
  ].map((t) => t.id);

  // Exactly once: a session in no bucket vanishes from the list, and one in
  // two buckets renders twice as if it were two different sessions.
  expect(placements.sort()).toEqual(all.map((s) => s.id).sort());
});

// The test above passed the whole time `archived` and `swept` were computed
// and then thrown away by the render, so an archived session — the only kind
// a human has explicitly finished — appeared nowhere at all. Covering the
// FUNCTION is not covering the LIST.
//
// This pins the set of buckets, so adding one forces a decision about where
// it renders instead of letting it be dropped silently. If this fails, either
// render the new bucket in SessionList's body or fold it into an existing group —
// do not just update the list here.
test("the bucket set is exactly what SessionList renders", () => {
  const b = bucketSessions([], new Set());

  expect(Object.keys(b).sort()).toEqual(
    ["archived", "finished", "needsYou", "quiet", "stalled", "swept", "working"].sort(),
  );
});

// A session with unanswered decisions is stalled until a human clicks, so it
// is the one thing that must never be filed under anything else.
//
// Keyed on liveState "blocked", not on the raw pendingDecisions count. The
// server derives blocked as live-AND-pending and checks it before working or
// stalled, so a live session with a decision outstanding always arrives here
// already labelled — a fixture pairing pendingDecisions with liveState
// "working" describes a state the server cannot emit, and the earlier version
// of this test asserted exactly that.
test("a blocked session outranks every other bucket", () => {
  const b = bucketSessions([session("a", { pendingDecisions: 1, liveState: "blocked" })], new Set());

  expect(b.needsYou.map((t) => t.id)).toEqual(["a"]);
  expect(b.working).toHaveLength(0);
});

// The other half of that rule, and the reason the count alone will not do.
// Nothing resolves a pending permission when its pod dies mid-decision, so the
// count stays above zero forever. Bucketing on it would pin that session to the
// top of the list permanently, offering allow/deny buttons that reach no pod —
// while DeriveLiveState, which checks liveness first, calls it not-live.
test("a session whose pod is gone is not in needsYou, however many decisions it left behind", () => {
  const b = bucketSessions([session("a", { pendingDecisions: 3, liveState: "idle" })], new Set());

  expect(b.needsYou).toHaveLength(0);
  expect(b.quiet.map((t) => t.id)).toEqual(["a"]);
});

// pendingDecisions is a COUNT, not the old awaiting_human boolean. Parallel
// tool calls each get their own pending permission, and answering one must
// not report the rest as resolved — the exact bug the boolean had.
test("a session stays in needsYou while any decision is still outstanding", () => {
  const twoLeft = bucketSessions([session("a", { pendingDecisions: 2, liveState: "blocked" })], new Set());
  expect(twoLeft.needsYou.map((t) => t.id)).toEqual(["a"]);

  const oneLeft = bucketSessions([session("a", { pendingDecisions: 1, liveState: "blocked" })], new Set());
  expect(oneLeft.needsYou.map((t) => t.id)).toEqual(["a"]);

  const noneLeft = bucketSessions([session("a", { pendingDecisions: 0, liveState: "idle" })], new Set());
  expect(noneLeft.needsYou).toHaveLength(0);
  expect(noneLeft.quiet.map((t) => t.id)).toEqual(["a"]);
});

// An archived session is finished by a human — the only terminal state the
// fleet has, because it is the only one a machine cannot compute. It must not
// also show up as something demanding attention.
test("an archived session is never in needsYou, even with a pending decision", () => {
  const b = bucketSessions([session("a", { pendingDecisions: 1, archivedAt: "2026-08-01T00:00:00Z" })], new Set());

  expect(b.needsYou).toHaveLength(0);
  expect(b.archived.map((t) => t.id)).toEqual(["a"]);
});

// A crashed pod is always news: nothing else tells the human their session
// died, now that there is no `failed` status to render.
test("a crashed session surfaces in finished rather than falling into quiet", () => {
  const b = bucketSessions([session("a", { podPhase: "POD_PHASE_CRASHED" })], new Set());

  expect(b.finished.map((t) => t.id)).toEqual(["a"]);
  expect(b.quiet).toHaveLength(0);
});

// swept means the retention GC reclaimed the disk: readable history, not
// resumable. The bucket exists so the row can render read-only — offering a
// Warm button would be an action that cannot succeed.
test("a swept session is reported as swept so no Warm button is offered", () => {
  const b = bucketSessions([session("a", { sweptAt: "2026-08-01T00:00:00Z", liveState: "idle" })], new Set());

  expect(b.swept.map((t) => t.id)).toEqual(["a"]);
});

// needsYouIds carries decisions the client knows about from the transcript
// before the server-side count catches up on the next poll.
test("needsYouIds alone is enough to pull a session into needsYou", () => {
  const b = bucketSessions([session("a", { liveState: "idle" })], new Set(["a"]));

  expect(b.needsYou.map((t) => t.id)).toEqual(["a"]);
  expect(b.quiet).toHaveLength(0);
});
