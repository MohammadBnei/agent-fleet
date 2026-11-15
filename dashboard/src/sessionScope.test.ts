import { expect, test } from "bun:test";
import { queueFor } from "./serialQueue";
import { keysOf, restoreDraft, scopeKey } from "./sessionScope";

const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));
const src = await Bun.file(
  new URL("./useSessionDetail.ts", import.meta.url),
).text();

// 1. A -> B -> A, or a layout remount, must land on the same queue.
test("a session keeps one send queue for the life of the page", async () => {
  expect(queueFor("A")).toBe(queueFor("A"));
  expect(queueFor("A")).not.toBe(queueFor("B"));
  const log: string[] = [];
  const first = queueFor("A")(async () => {
    await wait(15);
    log.push("first");
  });
  // "Remounted": fetched again, still behind the first.
  const second = queueFor("A")(async () => {
    log.push("second");
  });
  await Promise.all([first, second]);
  expect(log).toEqual(["first", "second"]);
});

// 2. Keys repeat across sessions; B must not see or collide with A's.
test("busy keys of another session are invisible", () => {
  const all = [
    scopeKey("A", "permission:7"),
    scopeKey("A", "action:warm"),
    scopeKey("B", "question:7"),
  ];
  expect(keysOf("B", all)).toEqual(["question:7"]);
  expect(keysOf("A", all)).toEqual(["permission:7", "action:warm"]);
  expect(scopeKey("A", "permission:7")).not.toBe(scopeKey("B", "permission:7"));
});

test("a late failure from a left session never reaches the new one", () => {
  const send = src.slice(src.indexOf("const sendDiscuss = useCallback"));
  const check = send.indexOf("idRef.current !== sessionId");
  const show = send.indexOf("setActionError(errorText(err))");
  expect(check).toBeGreaterThan(-1);
  expect(check).toBeLessThan(show);
  const run = src.slice(src.indexOf("const run = useCallback"));
  expect(run.slice(0, 600)).toContain("idRef.current === sessionId");
  expect(run.slice(0, 700)).toContain("scopeKey(sessionId, key)");
});

// 3. A failed message gives its text back.
test("a failed send is restored in front of the current draft", () => {
  expect(restoreDraft("hello", "")).toBe("hello");
  expect(restoreDraft("hello", "and more")).toBe("hello\nand more");
});

test("the composer restores text when the send resolves false", async () => {
  const view = await Bun.file(
    new URL("./useSessionView.ts", import.meta.url),
  ).text();
  expect(view).toContain("ok === false");
  expect(view).toContain("restoreDraft(text, m)");
  expect(src).toContain("return false;");
});
