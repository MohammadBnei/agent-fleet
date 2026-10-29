import { expect, test } from "bun:test";
import { answerText, displayNames, threadTitle, withOther } from "./text";

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
  const n = displayNames([
    "nadia@opus.tech",
    "sam@a.com",
    "sam@b.com",
    "",
    "Nadia",
  ]);
  expect(n.get("nadia@opus.tech")).toBe("nadia");
  expect(n.get("sam@a.com")).toBe("sam@a.com");
  expect(n.get("sam@b.com")).toBe("sam@b.com");
  expect(n.get("")).toBe("Another member");
  expect(n.get("Nadia")).toBe("Nadia");
});

test("typed text answers what was not picked", () => {
  expect(withOther(["A?", "B?"], { "A?": "yes" }, "my own")).toEqual({
    "A?": "yes",
    "B?": "my own",
  });
  expect(withOther(["A?"], {}, "free")).toEqual({ "A?": "free" });
});

test("answer entries read as their answers", () => {
  expect(
    answerText(JSON.stringify({ answers: { "A?": "yes", "B?": "my own" } })),
  ).toBe("yes\nmy own");
  expect(
    answerText(JSON.stringify({ answers: { "A?": "same", "B?": "same" } })),
  ).toBe("same");
  expect(answerText(JSON.stringify({ answers: { "A?": ["x"] } }))).toBe(
    '["x"]',
  );
  expect(answerText("not json")).toBe("not json");
});
