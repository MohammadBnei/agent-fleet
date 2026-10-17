import { test, expect } from "bun:test";
import { relativeTime } from "./relativeTime";

const now = Date.parse("2026-10-17T12:00:00Z");
const at = (minutesAgo: number) => new Date(now - minutesAgo * 60_000).toISOString();

test("relativeTime reads past, future and durations one way everywhere", () => {
  expect(relativeTime(at(0.5), { now })).toBe("just now");
  expect(relativeTime(at(5), { now })).toBe("5m ago");
  expect(relativeTime(at(150), { now })).toBe("2h ago");
  expect(relativeTime(at(60 * 26), { now })).toBe("1d ago");
  expect(relativeTime(at(-90), { now })).toBe("in 1h");
  expect(relativeTime(at(5), { now, bare: true })).toBe("5m");
  expect(relativeTime(at(0.2), { now, bare: true })).toBe("just now");
  expect(relativeTime(undefined)).toBeNull();
  expect(relativeTime("not a date")).toBeNull();
});
