import { expect, test } from "bun:test";
import { threadTitle } from "./text";

// CreateThread refuses titles over 200 bytes: multi-byte text must fit and
// never be cut mid-character.
test("titles fit in bytes and keep whole characters", () => {
  const enc = (s: string) => new TextEncoder().encode(s).length;
  expect(threadTitle("Plan the onboarding\nmore")).toBe("Plan the onboarding");
  const cjk = threadTitle("漢".repeat(100));
  expect(enc(cjk)).toBeLessThanOrEqual(120);
  expect(cjk).toBe("漢".repeat(40));
  const emoji = threadTitle("😀".repeat(100));
  expect(emoji).toBe("😀".repeat(30));
  expect(emoji.includes("�")).toBe(false);
});

test("a member is shown by the name part of their email", () => {
  const { displayName } = require("./text");
  expect(displayName("nadia@opus.tech")).toBe("nadia");
  expect(displayName("")).toBe("Member");
  expect(displayName("Nadia")).toBe("Nadia");
});
