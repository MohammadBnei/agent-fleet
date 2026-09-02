import { test, expect } from "bun:test";
import { statusDotClass } from "./pages/Schedules";

// A schedule that files nothing every tick, because the previous run still
// holds its dedup key, used to render the same success green as one that had
// just run — which is how a permanently-stalled schedule went unnoticed behind
// a healthy-looking dot. The distinction is three-way on purpose: a skip is not
// an error, because it is legitimate while the previous run is genuinely in
// flight.
test("a skipped tick is neither healthy nor an error", () => {
  expect(statusDotClass("skipped: session abc-123", true)).toBe("bg-dim2");
  expect(statusDotClass("skipped: previous run still open", true)).toBe("bg-dim2");
});

test("a filed proposal is healthy and a failed one is an error", () => {
  expect(statusDotClass("proposal abc-123", true)).toBe("bg-success");
  expect(statusDotClass("error: bad cron", true)).toBe("bg-warning");
});

// Paused wins over everything: a disabled schedule is not skipping, it is off.
test("a disabled schedule reads as paused whatever it last did", () => {
  expect(statusDotClass("skipped: session abc-123", false)).toBe("border border-dim2");
  expect(statusDotClass("error: bad cron", false)).toBe("border border-dim2");
});
