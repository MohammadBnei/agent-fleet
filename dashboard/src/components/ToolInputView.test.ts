import { test, expect } from "bun:test";
import { diffLines } from "./ToolInputView";

function render(before: string, after: string): string {
  return diffLines(before, after)
    .map((l) => `${l.kind === "add" ? "+" : l.kind === "remove" ? "-" : " "}${l.text}`)
    .join("\n");
}

test("an unchanged line stays context, not a remove plus an add", () => {
  expect(render("a\nb\nc", "a\nB\nc")).toBe(" a\n-b\n+B\n c");
});

test("a pure insertion keeps every original line as context", () => {
  expect(render("a\nc", "a\nb\nc")).toBe(" a\n+b\n c");
});

test("a pure deletion marks only the removed line", () => {
  expect(render("a\nb\nc", "a\nc")).toBe(" a\n-b\n c");
});

// A Write has no "before", so every line must read as added rather than
// as an unchanged block against an empty string.
test("an empty before makes the whole content an addition", () => {
  expect(render("", "x\ny")).toBe("+x\n+y");
});

test("identical input produces no change markers", () => {
  expect(diffLines("a\nb", "a\nb").every((l) => l.kind === "context")).toBe(true);
});

// Moved-block behaviour: LCS keeps the longest run, so the shorter
// duplicate is what shows as changed. Pinned because a naive line-by-line
// zip would instead mark everything after the move as different.
test("a moved line diffs against the longest common run", () => {
  expect(render("a\nb\nc\nd", "b\nc\nd\na")).toBe("-a\n b\n c\n d\n+a");
});
