import { test, expect } from "bun:test";

// v4.8.0 shipped multi-select and a per-row actions menu that the operator
// could not see at all. Every check passed: 85 unit tests, `bun run build`, and
// 11 Playwright assertions driving a real core on a kind cluster.
//
// The Playwright run seeded sessions into every bucket, so NEEDS YOU, STUCK,
// WORKING and FINISHED all rendered and all had their controls. A real fleet at
// rest has none of those — every session falls through to the quiet tail, which
// was the one row component built without a checkbox or a ⋯. So the console had
// nothing selectable and nothing actionable on it in its most common state, and
// the test suite agreed it was fine because the test fleet was never at rest.
//
// This greps the source, so be clear about what it proves: that each row
// component MENTIONS the controls, not that they are wired or that clicking one
// does anything. It exists to make "a new row kind shipped without actions" a
// test failure instead of a bug report. Same shape and same limits as
// core/internal/buildguard.
const SRC = await Bun.file(new URL("./pages/SessionList.tsx", import.meta.url)).text();
const MOBILE = await Bun.file(new URL("./mobile/MobileSessionList.tsx", import.meta.url)).text();
const MOBILE_DETAIL = await Bun.file(new URL("./mobile/MobileSessionDetail.tsx", import.meta.url)).text();
const PANELS = await Bun.file(new URL("./components/SessionPanels.tsx", import.meta.url)).text();
const APP = await Bun.file(new URL("./App.tsx", import.meta.url)).text();

// Slice out one `function Name(...)` block by finding the next top-level
// `function ` after it — good enough for a flat module of components.
function componentBody(src: string, name: string): string {
  const start = src.indexOf(`function ${name}(`);
  expect(start, `${name} not found — was it renamed?`).toBeGreaterThan(-1);
  const next = src.indexOf("\nfunction ", start + 1);
  return src.slice(start, next === -1 ? undefined : next);
}

// Every desktop row a session can land in. If a bucket gains a row component,
// add it here — bucketSessions.test.ts already forces the bucket itself to be
// rendered somewhere; this forces it to be actionable once it is.
const DESKTOP_ROWS = ["NeedsYouCard", "StuckRow", "FinishedRow", "WorkingRow", "CompactRow"];

// Both controls come from components/RowControls now; before that each row
// hand-rolled its own placement, which is how the phone ended up with a 20px
// tap target and the desktop with the checkbox in a different spot per row.
test.each(DESKTOP_ROWS)("%s offers selection and actions", (name) => {
  const body = componentBody(SRC, name);
  expect(body).toContain("RowSelect");
  expect(body).toContain("RowActionsButton");
});

// Mobile has no batch bar (no room, and bulk tidying is not a phone job), so
// only the actions menu is required there.
test.each(["NeedsYouCard", "FinishedCard", "WorkingCard", "StuckCard", "QuietRow"])(
  "mobile %s offers actions",
  (name) => {
    expect(componentBody(MOBILE, name)).toContain("RowActionsButton");
  },
);


// Mobile's only route to Interrupt/Kill/Warm/Archive/Mode/Delete is the
// "panels" sheet, and the sheet always carries the SESSION panel — so the
// opener must not be conditional on anything.
//
// It briefly was, gated on todos+changes+subagents all being empty. That took
// every action away from a session with no file changes, and because the gate
// read live state the button appeared when the agent wrote a todo and vanished
// when it cleared. Reported as "the mobile view is broken, I don't have access
// to the side panel" and then "oh, it went back".
test("mobile's panels opener is not conditional", () => {
  // Anchored on the CALL, not on the label. The first version of this keyed
  // off the literal "panels ▸" string; the button later became an icon, that
  // string survived only inside a code comment, and the test went on passing
  // against 600 characters of prose. A guard anchored on something cosmetic
  // stops guarding the moment the cosmetics change, and says nothing when it
  // does.
  const call = MOBILE_DETAIL.indexOf("setPanelsOpen(true)");
  expect(call, "the panels opener is gone entirely").toBeGreaterThan(-1);
  const opener = MOBILE_DETAIL.slice(Math.max(0, call - 700), call);
  expect(opener).not.toContain("panelsEmpty");
  // No && guard immediately wrapping the button either.
  expect(opener).not.toMatch(/&&\s*\(\s*<button/);
});

test("panelsEmpty is gone entirely, not just unused here", () => {
  expect(PANELS).not.toContain("export function panelsEmpty");
});


// The session list must fetch the NEWEST transcript entries, never a forward
// read from zero.
//
// core's transcriptWindow treats a missing limit as "read forward from
// sinceSeq", so `getTranscript({ sessionId, sinceSeq: 0n })` returned the
// OLDEST 1000 entries. Past that, this fetch could not see a pending decision
// at all — while core, counting over the whole table, still reported the
// session blocked. The console rendered a red blocked card with nothing in it,
// and a NEW question was the most invisible thing of all, landing at the high
// end of the transcript.
//
// Greps the source, so it proves the call shape and not the behaviour — the
// server-side window is covered by TestTranscriptWindow_ALimitedReadReturnsTheNewestEntries.
// It exists because this regresses by DELETING an argument, which reads like
// simplification.
test("the list's summary fetch asks for a bounded, newest-first page", () => {
  const call = APP.slice(APP.indexOf(".getTranscript("), APP.indexOf(".getTranscript(") + 200);
  expect(call, "getTranscript is gone from App.tsx — has the summary fetch moved?").toContain("getTranscript");
  expect(call).toContain("limit");
  expect(call).not.toContain("sinceSeq");
});
