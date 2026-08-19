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

test.each(DESKTOP_ROWS)("%s offers selection and actions", (name) => {
  const body = componentBody(SRC, name);
  expect(body).toContain("SelectBox");
  expect(body).toMatch(/RowActionsButton|onActions/);
});

// Mobile has no batch bar (no room, and bulk tidying is not a phone job), so
// only the actions menu is required there.
test.each(["NeedsYouCard", "FinishedCard", "WorkingCard", "StuckCard", "QuietRow"])(
  "mobile %s offers actions",
  (name) => {
    expect(componentBody(MOBILE, name)).toContain("ActionsDots");
  },
);
