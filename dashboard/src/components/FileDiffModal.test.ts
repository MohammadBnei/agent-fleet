import { test, expect } from "bun:test";
import { unifiedToLines } from "./FileDiffModal";

// git's own output for a one-line sed edit — the case this whole path exists
// for, since a sed has no captured tool input to replay.
const SED_DIFF = `diff --git a/a.txt b/a.txt
index 1234567..89abcde 100644
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three`;

test("a unified diff becomes the Line[] DiffLines already renders", () => {
  expect(unifiedToLines(SED_DIFF)).toEqual([
    { kind: "context", text: "@@ -1,3 +1,3 @@" },
    { kind: "context", text: "one" },
    { kind: "remove", text: "two" },
    { kind: "add", text: "TWO" },
    { kind: "context", text: "three" },
  ]);
});

// The trap: "+++ b/a.txt" and "--- a/a.txt" start with + and -, so a parser
// that checks those before the preamble renders the filename twice as a
// changed line — and pushes the real change below the fold.
test("the file preamble never renders as changed lines", () => {
  const texts = unifiedToLines(SED_DIFF).map((l) => l.text);
  expect(texts.some((t) => t.includes("a.txt"))).toBe(false);
});

// A new file has no `---`/`+++` pair worth showing but every line is an add.
test("a wholly new file is all additions", () => {
  const lines = unifiedToLines(`diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..e69de29
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+alpha
+beta`);
  expect(lines).toEqual([
    { kind: "context", text: "@@ -0,0 +1,2 @@" },
    { kind: "add", text: "alpha" },
    { kind: "add", text: "beta" },
  ]);
});

// "\ No newline at end of file" is git commentary, not a line of the file.
test("the no-newline marker is dropped", () => {
  expect(unifiedToLines("@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b")).toEqual([
    { kind: "context", text: "@@ -1 +1 @@" },
    { kind: "remove", text: "a" },
    { kind: "add", text: "b" },
  ]);
});

test("an empty diff yields no lines", () => {
  expect(unifiedToLines("")).toEqual([]);
});

// A deleted SQL/Lua comment starts with "-- ", which the `--- ` preamble rule
// matched — so the removal vanished and the diff showed only the line that
// replaced it. This repo's own db/migrations/*.sql is full of them.
test("content that looks like the preamble is not swallowed", () => {
  expect(
    unifiedToLines(`diff --git a/m.sql b/m.sql
--- a/m.sql
+++ b/m.sql
@@ -1,2 +1,2 @@
--- old comment
+-- new comment`),
  ).toEqual([
    { kind: "context", text: "@@ -1,2 +1,2 @@" },
    { kind: "remove", text: "-- old comment" },
    { kind: "add", text: "-- new comment" },
  ]);
});

// The mirror: a +++ / --- pair appearing INSIDE a hunk is content too.
test("a +++ line inside a hunk is content, not a header", () => {
  expect(unifiedToLines("@@ -1 +1 @@\n++++ banner\n---- banner")).toEqual([
    { kind: "context", text: "@@ -1 +1 @@" },
    { kind: "add", text: "+++ banner" },
    { kind: "remove", text: "--- banner" },
  ]);
});
