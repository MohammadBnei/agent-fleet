import { expect, test } from "bun:test";
import { displayNames, threadTitle } from "./text";

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

test("members are named by their email's local part, in full on a collision", () => {
  const n = displayNames(["nadia@opus.tech", "sam@a.com", "sam@b.com", "", "Nadia"]);
  expect(n.get("nadia@opus.tech")).toBe("nadia");
  expect(n.get("sam@a.com")).toBe("sam@a.com");
  expect(n.get("sam@b.com")).toBe("sam@b.com");
  expect(n.get("")).toBe("Another member");
  expect(n.get("Nadia")).toBe("Nadia");
});
